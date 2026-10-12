//go:build linux

package transparentproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/exp/slices"
	"golang.org/x/sys/unix"
)

const linuxOwnerProtocol = 197

// routeState lives only in this process. A fresh process discovers working
// routes from YAML and the actual capture rules; it never inherits sysctl
// originals from a previous process or reads/writes an ownership file.
type routeState struct {
	Policies []routePolicy
	Routes   []ownedRoute
	Rules    []ownedRule
	Sysctls  []ownedSysctl
}

type linuxRouteSystem struct {
	mu        sync.Mutex
	request   *wireConn
	state     routeState
	lease     int
	closeOnce sync.Once
	closeErr  error
	// Tests use a private proc tree and can model kernel sysctl side effects.
	procRoot          string
	beforeSysctlWrite func(path, value string) error
}

func newRouteSystem() (routeSystem, error) {
	// Linux abstract Unix sockets are scoped to the network namespace. One
	// fixed lease prevents two config homes from managing the same namespace,
	// without creating a filesystem socket or any other file.
	lease, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Bind(lease, &unix.SockaddrUnix{Name: "\x00mihomo-optimized.transparent-proxy"}); err != nil {
		_ = unix.Close(lease)
		return nil, fmt.Errorf("transparent proxy namespace is already managed: %w", err)
	}
	request, err := newWireConn(unix.NETLINK_ROUTE, 0)
	if err != nil {
		_ = unix.Close(lease)
		return nil, err
	}
	return &linuxRouteSystem{request: request, lease: lease, procRoot: "/proc/sys"}, nil
}

func (s *linuxRouteSystem) Close() error {
	s.closeOnce.Do(func() {
		if s.request != nil {
			s.closeErr = s.request.Close()
		}
		if s.lease >= 0 {
			s.closeErr = errors.Join(s.closeErr, unix.Close(s.lease))
		}
	})
	return s.closeErr
}

// Remember this process's exact resources before a kernel operation so a
// partial failure can be retried or cleaned up by the same process. Nothing is
// serialized or passed to a replacement process.
func (s *linuxRouteSystem) updateState(update func(*routeState)) error {
	previous := s.state
	next := previous
	next.Policies = slices.Clone(previous.Policies)
	next.Routes = slices.Clone(previous.Routes)
	next.Rules = slices.Clone(previous.Rules)
	next.Sysctls = slices.Clone(previous.Sysctls)
	update(&next)
	if err := validateRouteState(next); err != nil {
		return err
	}
	if slices.Equal(previous.Policies, next.Policies) && slices.Equal(previous.Routes, next.Routes) && slices.Equal(previous.Rules, next.Rules) && slices.Equal(previous.Sysctls, next.Sysctls) {
		return nil
	}
	s.state = next
	return nil
}

func (s *linuxRouteSystem) ipv6StackAvailable() (bool, error) {
	disabled, err := os.ReadFile(filepath.Join(s.procRoot, "net/ipv6/conf/all/disable_ipv6"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(string(disabled)) {
	case "0":
		return true, nil
	case "1":
		return false, nil
	default:
		return false, errors.New("invalid IPv6 disable state")
	}
}

func (s *linuxRouteSystem) Snapshot(ctx context.Context) (networkState, error) {
	var state networkState
	links, err := s.request.Execute(ctx, unix.RTM_GETLINK, unix.NLM_F_DUMP, make([]byte, 16))
	if err != nil {
		return state, fmt.Errorf("read network links: %w", err)
	}
	indices := make(map[int]int, len(links))
	for _, message := range links {
		if message.Type != unix.RTM_NEWLINK {
			return state, fmt.Errorf("%w: unexpected link dump type", errIncompleteNetlink)
		}
		link, err := decodeNetworkLink(message.Data)
		if err != nil {
			return state, err
		}
		if _, exists := indices[link.Index]; exists {
			return state, fmt.Errorf("%w: duplicate link", errIncompleteNetlink)
		}
		indices[link.Index] = len(state.Interfaces)
		state.Interfaces = append(state.Interfaces, link)
	}
	addresses, err := s.request.Execute(ctx, unix.RTM_GETADDR, unix.NLM_F_DUMP, make([]byte, 8))
	if err != nil {
		return state, fmt.Errorf("read network addresses: %w", err)
	}
	for _, message := range addresses {
		if message.Type != unix.RTM_NEWADDR {
			return state, fmt.Errorf("%w: unexpected address dump type", errIncompleteNetlink)
		}
		index, prefix, usable, err := decodeNetworkAddress(message.Data)
		if err != nil {
			return state, err
		}
		if !usable {
			continue
		}
		position, exists := indices[index]
		if !exists {
			return state, fmt.Errorf("%w: link changed during address dump", errIncompleteNetlink)
		}
		state.Interfaces[position].Addresses = append(state.Interfaces[position].Addresses, prefix)
	}
	for i := range state.Interfaces {
		slices.SortFunc(state.Interfaces[i].Addresses, func(a, b netip.Prefix) int {
			if order := a.Addr().Compare(b.Addr()); order != 0 {
				return order
			}
			return a.Bits() - b.Bits()
		})
		state.Interfaces[i].Addresses = slices.Compact(state.Interfaces[i].Addresses)
	}
	slices.SortFunc(state.Interfaces, func(a, b networkInterface) int { return a.Index - b.Index })
	state.IPv6Available, err = s.ipv6StackAvailable()
	if err != nil {
		return state, err
	}
	state.DefaultIPv4, err = s.defaultInterface(ctx, false, state.Interfaces)
	if err != nil {
		return state, err
	}
	if state.IPv6Available {
		state.DefaultIPv6, err = s.defaultInterface(ctx, true, state.Interfaces)
		if err != nil {
			return state, err
		}
	}
	return state, nil
}

func decodeNetworkLink(data []byte) (networkInterface, error) {
	var link networkInterface
	if len(data) < 16 {
		return link, fmt.Errorf("%w: short link", errIncompleteNetlink)
	}
	link.Index = int(int32(wireOrder.Uint32(data[4:8])))
	flags := wireOrder.Uint32(data[8:12])
	link.Up, link.Loopback = flags&unix.IFF_UP != 0, flags&unix.IFF_LOOPBACK != 0
	attributes, err := parseWireAttributes(data[16:])
	if err != nil {
		return link, err
	}
	for _, attribute := range attributes {
		if attribute.Type&wireAttributeTypeMask != unix.IFLA_IFNAME {
			continue
		}
		if link.Name != "" || len(attribute.Data) < 2 || len(attribute.Data) > unix.IFNAMSIZ || attribute.Data[len(attribute.Data)-1] != 0 {
			return link, fmt.Errorf("%w: invalid link name", errIncompleteNetlink)
		}
		link.Name = string(attribute.Data[:len(attribute.Data)-1])
		if strings.IndexByte(link.Name, 0) >= 0 {
			return link, fmt.Errorf("%w: embedded null in link name", errIncompleteNetlink)
		}
	}
	if link.Index <= 0 || link.Name == "" {
		return link, fmt.Errorf("%w: missing link identity", errIncompleteNetlink)
	}
	return link, nil
}

func decodeNetworkAddress(data []byte) (int, netip.Prefix, bool, error) {
	if len(data) < 8 {
		return 0, netip.Prefix{}, false, fmt.Errorf("%w: short address", errIncompleteNetlink)
	}
	family, bits, flags := data[0], int(data[1]), uint32(data[2])
	index := int(wireOrder.Uint32(data[4:8]))
	if family != unix.AF_INET && family != unix.AF_INET6 {
		return index, netip.Prefix{}, false, nil
	}
	attributes, err := parseWireAttributes(data[8:])
	if err != nil {
		return index, netip.Prefix{}, false, err
	}
	var address, local netip.Addr
	validLifetime := ^uint32(0)
	for _, attribute := range attributes {
		switch attribute.Type & wireAttributeTypeMask {
		case unix.IFA_ADDRESS, unix.IFA_LOCAL:
			value, ok := netip.AddrFromSlice(attribute.Data)
			if !ok || family == unix.AF_INET && !value.Is4() || family == unix.AF_INET6 && (!value.Is6() || value.Is4In6()) {
				return index, netip.Prefix{}, false, fmt.Errorf("%w: invalid address family", errIncompleteNetlink)
			}
			if attribute.Type&wireAttributeTypeMask == unix.IFA_LOCAL {
				local = value
			} else {
				address = value
			}
		case unix.IFA_FLAGS:
			if len(attribute.Data) != 4 {
				return index, netip.Prefix{}, false, fmt.Errorf("%w: invalid address flags", errIncompleteNetlink)
			}
			flags = wireOrder.Uint32(attribute.Data)
		case unix.IFA_CACHEINFO:
			if len(attribute.Data) != 16 {
				return index, netip.Prefix{}, false, fmt.Errorf("%w: invalid address lifetime", errIncompleteNetlink)
			}
			validLifetime = wireOrder.Uint32(attribute.Data[4:8])
		}
	}
	if local.IsValid() {
		address = local
	}
	prefix := netip.PrefixFrom(address, bits)
	if !prefix.IsValid() || index <= 0 {
		return index, prefix, false, fmt.Errorf("%w: missing address identity", errIncompleteNetlink)
	}
	usable := validLifetime != 0 && flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0
	return index, prefix, usable, nil
}

func (s *linuxRouteSystem) defaultInterface(ctx context.Context, ipv6 bool, links []networkInterface) (string, error) {
	// A route lookup asks the kernel's policy-routing engine, rather than
	// assuming the first default route in the main table is the active exit.
	destination := netip.MustParseAddr("1.1.1.1")
	family := byte(unix.AF_INET)
	if ipv6 {
		destination, family = netip.MustParseAddr("2606:4700:4700::1111"), unix.AF_INET6
	}
	data := make([]byte, 12)
	data[0], data[1] = family, byte(destination.BitLen())
	data = appendWireAttribute(data, unix.RTA_DST, destination.AsSlice())
	data = appendWireAttribute(data, unix.RTA_MARK, wireUint32(DefaultOutboundMark))
	messages, err := s.request.Execute(ctx, unix.RTM_GETROUTE, 0, data)
	if errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.ENODEV) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read default route: %w", err)
	}
	for _, message := range messages {
		if message.Type != unix.RTM_NEWROUTE || len(message.Data) < 12 {
			return "", fmt.Errorf("%w: invalid route lookup", errIncompleteNetlink)
		}
		if message.Data[7] != unix.RTN_UNICAST {
			continue
		}
		attributes, err := parseWireAttributes(message.Data[12:])
		if err != nil {
			return "", err
		}
		for _, attribute := range attributes {
			if attribute.Type&wireAttributeTypeMask != unix.RTA_OIF {
				continue
			}
			if len(attribute.Data) != 4 {
				return "", fmt.Errorf("%w: invalid route interface", errIncompleteNetlink)
			}
			index := int(wireOrder.Uint32(attribute.Data))
			for _, link := range links {
				if link.Index == index && link.Up && !link.Loopback {
					return link.Name, nil
				}
			}
		}
	}
	return "", nil
}

type linuxSystemWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (w *linuxSystemWatch) Close() error { w.cancel(); <-w.done; return nil }

func (s *linuxRouteSystem) Watch(ctx context.Context) (<-chan systemEvent, io.Closer, error) {
	groups := uint32(0)
	for _, group := range []uint32{unix.RTNLGRP_LINK, unix.RTNLGRP_IPV4_IFADDR, unix.RTNLGRP_IPV6_IFADDR, unix.RTNLGRP_IPV4_ROUTE, unix.RTNLGRP_IPV6_ROUTE, unix.RTNLGRP_IPV4_RULE, unix.RTNLGRP_IPV6_RULE, unix.RTNLGRP_IPV4_NETCONF, unix.RTNLGRP_IPV6_NETCONF} {
		groups |= 1 << (group - 1)
	}
	routes, err := newWireConn(unix.NETLINK_ROUTE, groups)
	if err != nil {
		return nil, nil, err
	}
	nft, err := newWireConn(unix.NETLINK_NETFILTER, 1<<(unix.NFNLGRP_NFTABLES-1))
	if err != nil {
		_ = routes.Close()
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	watch := &linuxSystemWatch{cancel: cancel, done: make(chan struct{})}
	events := make(chan systemEvent, 1)
	dirty := make(chan struct{}, 1)
	failed := make(chan error, 2)
	var readers sync.WaitGroup
	readers.Add(2)
	read := func(connection *wireConn, nftables bool) {
		defer readers.Done()
		for {
			packets, err := connection.receive(ctx)
			if err != nil {
				if ctx.Err() == nil {
					select {
					case failed <- err:
					case <-ctx.Done():
					}
				}
				return
			}
			changed := false
			for _, packet := range packets {
				change, err := validateSystemNotification(packet, nftables)
				if err != nil {
					select {
					case failed <- err:
					case <-ctx.Done():
					}
					return
				}
				changed = changed || change
			}
			if changed {
				select {
				case dirty <- struct{}{}:
				default:
				}
			}
		}
	}
	go read(routes, false)
	go read(nft, true)
	go func() {
		defer close(watch.done)
		defer close(events)
		defer func() { cancel(); _ = routes.Close(); _ = nft.Close(); readers.Wait() }()
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-failed:
				// Deliver the fault even when an ordinary invalidation was queued.
				// The consumer must reopen subscriptions before trusting a dump.
				select {
				case <-events:
				default:
				}
				select {
				case events <- systemEvent{Err: err}:
				case <-ctx.Done():
				}
				return
			case <-dirty:
				// Coalescing is lossless: an event means reread the whole state,
				// never apply a partial delta from this bounded notification queue.
				select {
				case events <- systemEvent{}:
				default:
				}
			}
		}
	}()
	return events, watch, nil
}

func validateSystemNotification(packet wirePacket, nftables bool) (bool, error) {
	if err := checkWirePacket(packet); err != nil {
		return false, err
	}
	if packet.Type == unix.NLMSG_NOOP || packet.Type == unix.NLMSG_DONE || packet.Type == unix.NLMSG_ERROR {
		return false, nil
	}
	if nftables {
		if packet.Type>>8 != unix.NFNL_SUBSYS_NFTABLES {
			return false, nil
		}
		if len(packet.Data) < 4 {
			return false, fmt.Errorf("%w: short nft notification", errIncompleteNetlink)
		}
		attributes, err := parseWireAttributes(packet.Data[4:])
		if err != nil {
			return false, err
		}
		if packet.Type&0xff != unix.NFT_MSG_NEWGEN {
			return false, nil
		}
		for _, attribute := range attributes {
			if attribute.Type&wireAttributeTypeMask == unix.NFTA_GEN_ID && len(attribute.Data) == 4 {
				return true, nil
			}
		}
		return false, fmt.Errorf("%w: nft transaction without generation", errIncompleteNetlink)
	}
	switch packet.Type {
	case unix.RTM_NEWLINK, unix.RTM_DELLINK:
		_, err := decodeNetworkLink(packet.Data)
		return err == nil, err
	case unix.RTM_NEWADDR, unix.RTM_DELADDR:
		_, _, _, err := decodeNetworkAddress(packet.Data)
		return err == nil, err
	case unix.RTM_NEWROUTE, unix.RTM_DELROUTE, unix.RTM_NEWRULE, unix.RTM_DELRULE:
		if len(packet.Data) < 12 {
			return false, fmt.Errorf("%w: short route/rule notification", errIncompleteNetlink)
		}
		_, err := parseWireAttributes(packet.Data[12:])
		return err == nil, err
	case unix.RTM_NEWNETCONF, unix.RTM_DELNETCONF:
		if len(packet.Data) < 4 {
			return false, fmt.Errorf("%w: short network configuration notification", errIncompleteNetlink)
		}
		_, err := parseWireAttributes(packet.Data[4:])
		return err == nil, err
	}
	return false, nil
}

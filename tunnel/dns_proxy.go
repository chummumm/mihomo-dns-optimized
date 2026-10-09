package tunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/metacubex/mihomo/component/dnsmessage"
	R "github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/tunnel/statistic"

	"github.com/miekg/dns"
)

const (
	dnsProxyTimeout    = 5 * time.Second
	dnsProxyMaxDepth   = 64
	dnsProxyMaxMessage = dnsmessage.MaxSize
)

var errDNSProxyDrop = errors.New("DNS proxy query rejected without a response")

type dnsProxyRoute struct {
	proxy C.ProxyAdapter
	rule  C.Rule
	qname string
	// Groups are chosen using QNAME and frozen for this one exchange. Dialing
	// the group with resolver metadata would hash the DNS server's IP instead.
	groups []C.ProxyAdapter
	// Capability checks are part of the same selection snapshot. A later
	// change to a selector must not affect this query's frozen transport.
	udpSupportFrozen bool
	udpSupportError  error
}

type dnsProxySelect func(*C.Metadata) (dnsProxyRoute, error)
type dnsProxyExchange func(context.Context, []byte, *C.Metadata, dnsProxyRoute) ([]byte, error)

// ExchangeDNS routes one DNS question through the live rules with its real
// inbound/source metadata. The queried site's destination IP remains unknown;
// the resolver IP is used only for transport, never as the site's address.
//
// Call once per UDP datagram or DNS-over-TCP message, not once per connection.
// Resolver hostnames use a separate bootstrap resolver, never QNAME routing.
func (t tunnel) ExchangeDNS(ctx context.Context, query []byte, resolver *C.Metadata) ([]byte, error) {
	return exchangeDNSProxy(ctx, query, resolver, selectDNSProxy, exchangeDNSProxyWire)
}

var _ C.DNSExchanger = Tunnel

func exchangeDNSProxy(ctx context.Context, wire []byte, resolver *C.Metadata, selectProxy dnsProxySelect, exchange dnsProxyExchange) ([]byte, error) {
	if resolver == nil || resolver.DstPort != 53 {
		return nil, errors.New("DNS query routing requires destination port 53")
	}
	if resolver.NetWork != C.TCP && resolver.NetWork != C.UDP {
		return nil, errors.New("DNS proxy requires TCP or UDP")
	}
	if resolver.Host == "" || resolver.DstIP.IsValid() {
		if !(resolver.DstIP.IsGlobalUnicast() || resolver.DstIP.IsLoopback() || resolver.DstIP.IsLinkLocalUnicast()) {
			return nil, errors.New("DNS query routing requires a unicast resolver address")
		}
	}
	request, err := dnsmessage.UnpackQuery(wire)
	if err != nil {
		return nil, err
	}
	question := request.Question[0]
	host := strings.ToLower(strings.TrimSuffix(question.Name, "."))
	// The root name is a valid DNS question and should reach MATCH.
	if host == "" {
		host = "."
	}
	ctx, cancel := context.WithTimeout(ctx, dnsProxyTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	metadata := dnsRoutingMetadata(host, resolver)
	route, err := selectProxy(metadata)
	if err != nil {
		return nil, err
	}
	route.qname = host
	if route.proxy == nil {
		return nil, errors.New("DNS proxy selected an unavailable outbound")
	}
	switch route.proxy.Type() {
	case C.Reject:
		reply := new(dns.Msg).SetRcode(request, dns.RcodeRefused)
		return reply.Pack()
	case C.RejectDrop:
		return nil, errDNSProxyDrop
	case C.Dns, C.Rematch, C.Pass, C.PassRule:
		return nil, fmt.Errorf("DNS proxy cannot exchange through outbound type %s", route.proxy.Type())
	}
	if err := validateDNSRouteTransport(route, resolver.NetWork); err != nil {
		return nil, err
	}
	destination := resolver.Clone()
	if !destination.DstIP.IsValid() && destination.Host != "" {
		bootstrap := icontext.WithDNSBootstrap(ctx)
		ip, err := R.ResolveIPWithResolver(bootstrap, destination.Host, R.ProxyServerHostResolver)
		if err != nil {
			return nil, fmt.Errorf("DNS resolver bootstrap: %w", err)
		}
		destination.DstIP = ip
	}
	if !(destination.DstIP.IsGlobalUnicast() || destination.DstIP.IsLoopback() || destination.DstIP.IsLinkLocalUnicast()) {
		return nil, errors.New("DNS query routing requires a unicast resolver address")
	}
	destination.DstIP = destination.DstIP.Unmap()
	destination.Process, destination.ProcessPath, destination.Uid = metadata.Process, metadata.ProcessPath, metadata.Uid
	destination.Host = ""
	destination.SniffHost = ""
	destination.DNSMode = C.DNSNormal
	destination.SpecialProxy = ""
	destination.SpecialRules = ""
	response, err := exchange(ctx, wire, destination, route)
	if err != nil {
		return nil, err
	}
	if err := validateDNSProxyResponse(request, response); err != nil {
		return nil, err
	}
	chain := route.proxy.Name()
	for index := len(route.groups) - 1; index >= 0; index-- {
		chain = route.groups[index].Name() + "[" + chain + "]"
	}
	if route.rule != nil {
		log.Debugln("[DNS proxy] %s (%s) --> %s match %s(%s) using %s", host, dns.TypeToString[question.Qtype], destination.RemoteAddress(), route.rule.RuleType(), route.rule.Payload(), chain)
	} else {
		log.Debugln("[DNS proxy] %s (%s) --> %s using %s", host, dns.TypeToString[question.Qtype], destination.RemoteAddress(), chain)
	}
	return response, nil
}

func validateDNSProxyResponse(request *dns.Msg, wire []byte) error {
	response, err := dnsmessage.Unpack(wire)
	if err != nil {
		return err
	}
	if !response.Response || response.Opcode != request.Opcode || response.Id != request.Id {
		return errors.New("DNS proxy received a mismatched response")
	}
	// Some resolvers return only the header for errors such as REFUSED or
	// FORMERR. Strict Unpack already verified that every section count is zero.
	// Preserve that error; a successful answer must still echo the question.
	if len(wire) == 12 && response.Rcode != dns.RcodeSuccess {
		return nil
	}
	if len(response.Question) != 1 {
		return errors.New("DNS proxy received a mismatched response")
	}
	question, answerQuestion := request.Question[0], response.Question[0]
	if !strings.EqualFold(question.Name, answerQuestion.Name) || question.Qtype != answerQuestion.Qtype || question.Qclass != answerQuestion.Qclass {
		return errors.New("DNS proxy received a response for a different question")
	}
	return nil
}

func exchangeDNSProxyWire(ctx context.Context, query []byte, resolver *C.Metadata, route dnsProxyRoute) ([]byte, error) {
	ctx = icontext.WithDNSFixedOutbound(icontext.WithDNSRoutingMetadata(ctx, resolver), route.proxy)
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("DNS proxy exchange requires a deadline")
	}
	request, err := dnsmessage.UnpackQuery(query)
	if err != nil {
		return nil, err
	}
	if resolver.NetWork == C.TCP {
		conn, err := route.proxy.DialContext(ctx, resolver)
		if err != nil {
			return nil, err
		}
		appendDNSProxyGroups(conn, route.groups)
		closeState := newDNSQueryCloseState(ctx)
		if closeState != nil {
			conn = &dnsQueryNotifyConn{Conn: conn, state: closeState}
		}
		conn = statistic.NewTCPTracker(conn, statistic.DefaultManager, dnsProxyDisplayMetadata(resolver, route.qname), route.rule, 0, 0, true)
		normalClose := dnsQueryNormalCloser{close: conn.Close, state: closeState}
		defer normalClose.Close()
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
		stop := closeDNSProxyOnCancel(ctx, normalClose)
		defer stop()
		frame := make([]byte, len(query)+2)
		binary.BigEndian.PutUint16(frame, uint16(len(query)))
		copy(frame[2:], query)
		if err := writeDNSProxyFrame(conn, frame); err != nil {
			return nil, err
		}
		var length [2]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return nil, err
		}
		n := int(binary.BigEndian.Uint16(length[:]))
		if n < 12 {
			return nil, errors.New("DNS proxy received an invalid TCP frame length")
		}
		response := make([]byte, n)
		if _, err := io.ReadFull(conn, response); err != nil {
			return nil, err
		}
		return response, nil
	}
	conn, err := route.proxy.ListenPacketContext(ctx, resolver)
	if err != nil {
		return nil, err
	}
	appendDNSProxyGroups(conn, route.groups)
	closeState := newDNSQueryCloseState(ctx)
	if closeState != nil {
		conn = &dnsQueryNotifyPacketConn{PacketConn: conn, state: closeState}
	}
	conn = statistic.NewUDPTracker(conn, statistic.DefaultManager, dnsProxyDisplayMetadata(resolver, route.qname), route.rule, 0, 0, true)
	normalClose := dnsQueryNormalCloser{close: conn.Close, state: closeState}
	defer normalClose.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	stop := closeDNSProxyOnCancel(ctx, normalClose)
	defer stop()
	if n, err := conn.WriteTo(query, resolver.UDPAddr()); err != nil {
		return nil, err
	} else if n != len(query) {
		return nil, io.ErrShortWrite
	}
	response := make([]byte, dnsProxyMaxMessage)
	for {
		n, from, err := conn.ReadFrom(response)
		if err != nil {
			return nil, err
		}
		if !sameDNSProxyEndpoint(from, resolver.AddrPort()) {
			// Ignore unrelated packets until the bounded exchange deadline.
			continue
		}
		if err := validateDNSProxyResponse(request, response[:n]); err != nil {
			// A delayed or unrelated datagram from this resolver must not end
			// the current exchange before its matching reply can arrive.
			continue
		}
		return response[:n], nil
	}
}

// Group DialContext/ListenPacketContext normally append themselves while
// returning from the inner dial. DNS routing freezes the leaf using QNAME, so
// restore that same inner-to-outer chain without selecting the groups again.
func appendDNSProxyGroups(conn C.Connection, groups []C.ProxyAdapter) {
	for index := len(groups) - 1; index >= 0; index-- {
		conn.AppendToChains(groups[index])
	}
}

func dnsProxyDisplayMetadata(resolver *C.Metadata, qname string) *C.Metadata {
	metadata := resolver.Clone()
	// QNAME is only a display/routing label. Never put it on the metadata used
	// by the outbound adapter: the actual destination remains resolver IP:53.
	metadata.Host = qname
	return metadata
}

func writeDNSProxyFrame(w io.Writer, frame []byte) error {
	for len(frame) > 0 {
		n, err := w.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}

// Also interrupt blocked reads promptly when the caller cancels (for example
// when an inbound is closed), rather than waiting for the complete per-query
// deadline. Uses Go 1.20-compatible APIs.
func closeDNSProxyOnCancel(ctx context.Context, conn io.Closer) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func sameDNSProxyEndpoint(addr net.Addr, expected netip.AddrPort) bool {
	if addr == nil {
		return false
	}
	metadata := new(C.Metadata)
	if err := metadata.SetRemoteAddr(addr); err != nil || !metadata.DstIP.IsValid() {
		return false
	}
	return metadata.AddrPort() == expected
}

func selectDNSProxy(metadata *C.Metadata) (dnsProxyRoute, error) {
	return selectDNSProxyWithOptions(metadata, false)
}

func selectDNSProxyWithOptions(metadata *C.Metadata, deferUDPCheck bool, processOrigin ...*C.Metadata) (dnsProxyRoute, error) {
	return selectDNSProxyWithProcessSnapshot(metadata, deferUDPCheck, false, processOrigin...)
}

func selectDNSProxyWithProcessSnapshot(metadata *C.Metadata, deferUDPCheck, processSnapshot bool, processOrigin ...*C.Metadata) (dnsProxyRoute, error) {
	var proxy C.Proxy
	var rule C.Rule
	var err error
	name := metadata.SpecialProxy
	if name == "" {
		switch Mode() {
		case Direct:
			name = "DIRECT"
		case Global:
			name = "GLOBAL"
		}
	}
	if name != "" {
		configMux.RLock()
		proxy = proxies[name]
		configMux.RUnlock()
	} else {
		helper := newRuleMatchHelperWithProcessSnapshot(metadata, false, processSnapshot, processOrigin...)
		proxy, rule, err = matchWithOptions(metadata, helper, ruleMatchOptions{
			dnsQuery: true, deferUDPCheck: deferUDPCheck,
			evaluate: func(rule C.Rule, metadata *C.Metadata, helper C.RuleMatchHelper) (bool, string) {
				result := matchDNSProxyRuleContext(rule, metadata, dnsRuleContext{helper: helper}, 0)
				return result.known && result.match, result.adapter
			},
		})
	}
	if err != nil {
		return dnsProxyRoute{}, err
	}
	return unwrapDNSProxyRoute(proxy, rule, metadata, true, !deferUDPCheck)
}

func unwrapDNSProxy(proxy C.ProxyAdapter, rule C.Rule, metadata *C.Metadata) (dnsProxyRoute, error) {
	return unwrapDNSProxyWithTouch(proxy, rule, metadata, true)
}

func unwrapDNSProxyWithTouch(proxy C.ProxyAdapter, rule C.Rule, metadata *C.Metadata, touch bool) (dnsProxyRoute, error) {
	return unwrapDNSProxyRoute(proxy, rule, metadata, touch, touch)
}

func unwrapDNSProxyRoute(proxy C.ProxyAdapter, rule C.Rule, metadata *C.Metadata, touch, checkUDP bool) (dnsProxyRoute, error) {
	route := dnsProxyRoute{proxy: proxy, rule: rule}
	for depth := 0; depth < dnsProxyMaxDepth; depth++ {
		if route.proxy == nil {
			return route, errors.New("DNS proxy outbound is unavailable")
		}
		if touch && route.udpSupportError == nil && !route.proxy.SupportUDP() {
			route.udpSupportError = fmt.Errorf("DNS proxy outbound %q does not support UDP", route.proxy.Name())
		}
		if checkUDP && metadata.NetWork == C.UDP && route.udpSupportError != nil {
			return route, route.udpSupportError
		}
		next := route.proxy.Unwrap(metadata, touch)
		if next == nil {
			route.udpSupportFrozen = touch
			return route, nil
		}
		route.groups = append(route.groups, route.proxy)
		route.proxy = next
	}
	return route, errors.New("DNS proxy outbound group nesting is too deep or cyclic")
}

// Unknown is not false: NOT(IP-CIDR,...) must not become true just because DNS
// routing deliberately did not resolve the destination IP. The same rule
// applies to mixed classical providers and nested logical conditions.
type dnsProxyMatch struct {
	match   bool
	known   bool
	adapter string
}

type dnsProxyRuleChildren interface {
	Rules() []C.Rule
}

type dnsRuleContext struct {
	helper C.RuleMatchHelper
	// RULE-SET,src swaps the destination and source fields. The unknown
	// website IP follows that swap; actual source addresses stay usable.
	swapped bool
}

func matchDNSProxyRule(rule C.Rule, metadata *C.Metadata, depth int) dnsProxyMatch {
	return matchDNSProxyRuleContext(rule, metadata, dnsRuleContext{}, depth)
}

func matchDNSProxyRuleContext(rule C.Rule, metadata *C.Metadata, evaluation dnsRuleContext, depth int) dnsProxyMatch {
	if depth >= dnsProxyMaxDepth {
		return dnsProxyMatch{}
	}
	if wrapped, ok := rule.(C.RuleWrapper); ok {
		if wrapped.IsDisabled() {
			return dnsProxyMatch{known: true}
		}
		result := matchDNSProxyRuleContext(wrapped.Unwrap(), metadata, evaluation, depth+1)
		if count, ok := wrapped.(interface {
			Hit()
			Miss()
		}); ok && result.known {
			if result.match {
				count.Hit()
			} else {
				count.Miss()
			}
		}
		return result
	}
	switch rule.RuleType() {
	case C.IPCIDR, C.GEOIP, C.IPASN, C.IPSuffix:
		if !evaluation.swapped {
			return dnsProxyMatch{}
		}
		matched, adapter := rule.Match(metadata, evaluation.helper)
		return dnsProxyMatch{match: matched, known: true, adapter: adapter}
	case C.SrcIPCIDR, C.SrcGEOIP, C.SrcIPASN, C.SrcIPSuffix:
		if evaluation.swapped {
			return dnsProxyMatch{}
		}
		matched, adapter := rule.Match(metadata, evaluation.helper)
		return dnsProxyMatch{match: matched, known: true, adapter: adapter}
	case C.Domain, C.DomainSuffix, C.DomainKeyword, C.DomainRegex, C.DomainWildcard, C.GEOSITE, C.MATCH,
		C.SrcPort, C.DstPort, C.InPort, C.Network, C.InName, C.InType, C.InUser, C.DSCP,
		C.ProcessName, C.ProcessPath, C.ProcessNameRegex, C.ProcessPathRegex,
		C.ProcessNameWildcard, C.ProcessPathWildcard, C.Uid, C.RematchName:
		matched, adapter := rule.Match(metadata, evaluation.helper)
		return dnsProxyMatch{match: matched, known: true, adapter: adapter}
	case C.RuleSet:
		provider, ok := ruleProviders[rule.Payload()]
		if !ok {
			return dnsProxyMatch{}
		}
		if source, ok := rule.(interface{ SourceIP() bool }); ok && source.SourceIP() {
			original := metadata
			metadata = metadata.Clone()
			metadata.SwapSrcDst()
			evaluation.swapped = !evaluation.swapped
			if find := evaluation.helper.FindProcess; find != nil {
				// Process lookup belongs to the actual connection, even when
				// a RULE-SET swaps IP/port fields for matching purposes.
				evaluation.helper.FindProcess = func() {
					find()
					metadata.Process, metadata.ProcessPath, metadata.Uid = original.Process, original.ProcessPath, original.Uid
				}
			}
		}
		switch provider.Behavior() {
		case P.Domain:
			return dnsProxyMatch{match: provider.Match(metadata, evaluation.helper), known: true, adapter: rule.Adapter()}
		case P.IPCIDR:
			if evaluation.swapped {
				return dnsProxyMatch{match: provider.Match(metadata, evaluation.helper), known: true, adapter: rule.Adapter()}
			}
		case P.Classical:
			if children, ok := provider.Strategy().(dnsProxyRuleChildren); ok {
				result := matchDNSProxyChildrenContext(children.Rules(), C.OR, metadata, evaluation, depth+1)
				result.adapter = rule.Adapter()
				return result
			}
		}
	case C.AND, C.OR, C.NOT:
		if children, ok := rule.(dnsProxyRuleChildren); ok {
			result := matchDNSProxyChildrenContext(children.Rules(), rule.RuleType(), metadata, evaluation, depth+1)
			result.adapter = rule.Adapter()
			return result
		}
	case C.SubRules:
		if children, ok := rule.(dnsProxyRuleChildren); ok && len(children.Rules()) == 1 {
			condition := matchDNSProxyRuleContext(children.Rules()[0], metadata, evaluation, depth+1)
			if !condition.known || !condition.match {
				return condition
			}
			for _, child := range subRules[rule.Adapter()] {
				result := matchDNSProxyRuleContext(child, metadata, evaluation, depth+1)
				if !result.known || !result.match {
					continue
				}
				if proxy, ok := proxies[result.adapter]; ok {
					route, err := unwrapDNSProxyWithTouch(proxy, child, metadata, false)
					if err == nil && route.proxy.Type() == C.PassRule {
						continue
					}
				}
				return result
			}
			return dnsProxyMatch{known: true}
		}
	}
	return dnsProxyMatch{}
}

func matchDNSProxyChildrenContext(children []C.Rule, ruleType C.RuleType, metadata *C.Metadata, evaluation dnsRuleContext, depth int) dnsProxyMatch {
	if ruleType == C.NOT {
		if len(children) != 1 {
			return dnsProxyMatch{}
		}
		result := matchDNSProxyRuleContext(children[0], metadata, evaluation, depth)
		if result.known {
			result.match = !result.match
		}
		return result
	}
	unknown := false
	for _, child := range children {
		result := matchDNSProxyRuleContext(child, metadata, evaluation, depth)
		if !result.known {
			unknown = true
			continue
		}
		if ruleType == C.OR && result.match {
			return dnsProxyMatch{match: true, known: true}
		}
		if ruleType == C.AND && !result.match {
			return dnsProxyMatch{known: true}
		}
	}
	return dnsProxyMatch{match: ruleType == C.AND && !unknown, known: !unknown}
}

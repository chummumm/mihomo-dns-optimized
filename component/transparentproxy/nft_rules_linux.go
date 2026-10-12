//go:build linux

package transparentproxy

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/metacubex/nftables"
	"github.com/metacubex/nftables/binaryutil"
	"github.com/metacubex/nftables/expr"
	"github.com/metacubex/nftables/userdata"
	"go4.org/netipx"
	"golang.org/x/sys/unix"
)

const (
	nftTableName = "mihomo_optimized_tp"
	// This identifies working firewall objects, not a serialized recovery
	// record. One namespace lease excludes concurrent module instances.
	nftModule = "mihomo-optimized/transparent-proxy/v1"
)

type nftDesiredSet struct {
	set      *nftables.Set
	elements []nftables.SetElement
}

type nftDesiredChain struct {
	chain *nftables.Chain
	rules []*nftables.Rule
}

type nftRuleset struct {
	table  *nftables.Table
	sets   []*nftDesiredSet
	chains []*nftDesiredChain
}

func buildNFTRuleset(state firewallState) (*nftRuleset, error) {
	for _, mode := range []Mode{state.TCP, state.UDP} {
		if mode != ModeOff && mode != ModeTUN && mode != ModeTProxy && mode != ModeRedir {
			return nil, fmt.Errorf("invalid transparent-proxy capture mode %q", mode)
		}
	}
	if state.UDP == ModeRedir {
		return nil, errors.New("transparent-proxy REDIRECT only supports TCP")
	}
	usesTProxy := state.TCP == ModeTProxy || state.UDP == ModeTProxy
	usesTUN := state.TCP == ModeTUN || state.UDP == ModeTUN
	if usesTProxy && (state.TProxyMark == 0 || state.TProxyMark == RedirMark) || usesTUN && (state.TunMark == 0 || state.TunMark == RedirMark) || usesTProxy && usesTUN && state.TProxyMark == state.TunMark {
		return nil, errors.New("transparent-proxy capture marks must be nonzero and distinct")
	}
	for _, mark := range state.BypassMarks {
		if mark == RedirMark || usesTProxy && mark == state.TProxyMark || usesTUN && mark == state.TunMark {
			return nil, fmt.Errorf("transparent-proxy bypass mark %#x conflicts with capture", mark)
		}
	}
	if usesTProxy && state.TProxyPort == 0 && (state.TProxyTCP4 || state.TProxyTCP6 || state.TProxyUDP4 || state.TProxyUDP6) {
		return nil, errors.New("ready transparent-proxy TProxy listener has no port")
	}
	if state.TCP == ModeRedir && state.RedirPort == 0 && (state.Redir4 || state.Redir6) {
		return nil, errors.New("ready transparent-proxy REDIRECT listener has no port")
	}
	if state.TunName != "" && !validNFTInterface(state.TunName) {
		return nil, fmt.Errorf("invalid transparent-proxy TUN interface %q", state.TunName)
	}
	if usesTUN && (state.Tun4 || state.Tun6) && state.TunName == "" {
		return nil, errors.New("ready transparent-proxy TUN has no interface")
	}

	ruleset := &nftRuleset{
		table: &nftables.Table{Name: nftTableName, Family: nftables.TableFamilyINet},
	}
	addSet := func(name string, kind nftables.SetDatatype, elements []nftables.SetElement, interval bool) *nftables.Set {
		set := &nftables.Set{
			Table: ruleset.table, Name: name, ID: uint32(len(ruleset.sets) + 1),
			KeyType: kind, Interval: interval, Comment: nftModule + "/set/" + name,
		}
		ruleset.sets = append(ruleset.sets, &nftDesiredSet{set: set, elements: normalizeNFTElements(elements)})
		return set
	}
	interfaces := make(map[string]*nftables.Set)
	for _, entry := range []struct {
		name  string
		names []string
	}{{"local4", state.Local4}, {"local6", state.Local6}, {"gateway", state.Gateway}} {
		var elements []nftables.SetElement
		all := false
		for _, name := range entry.names {
			if name == "*" {
				all = true
				continue
			}
			if !validNFTInterface(name) {
				return nil, fmt.Errorf("invalid transparent-proxy interface %q", name)
			}
			elements = append(elements, nftables.SetElement{Key: nftInterfaceName(name)})
		}
		set := addSet(entry.name, nftables.TypeIFName, elements, false)
		if !all {
			interfaces[entry.name] = set
		}
	}
	uidSet := addSet("bypass_uids", nftables.TypeUID, nftUint32Elements(state.BypassUIDs), false)
	markSet := addSet("bypass_marks", nftables.TypeMark, nftUint32Elements(state.BypassMarks), false)
	portSets := make(map[byte]*nftables.Set)
	for _, entry := range []struct {
		name     string
		protocol byte
		ports    []uint16
	}{{"bypass_tcp_ports", unix.IPPROTO_TCP, state.BypassTCPPorts}, {"bypass_udp_ports", unix.IPPROTO_UDP, state.BypassUDPPorts}} {
		var elements []nftables.SetElement
		for _, port := range entry.ports {
			if port == 0 {
				return nil, errors.New("transparent-proxy bypass ports must be 1..65535")
			}
			elements = append(elements, nftables.SetElement{Key: binaryutil.BigEndian.PutUint16(port)})
		}
		portSets[entry.protocol] = addSet(entry.name, nftables.TypeInetService, elements, false)
	}
	localTypeSet := addSet("local_types", nftables.TypeInteger, nftUint32Elements([]uint32{unix.RTN_LOCAL, unix.RTN_BROADCAST, unix.RTN_ANYCAST, unix.RTN_MULTICAST}), false)
	addressSets := make(map[string]*nftables.Set)
	for _, entry := range []struct {
		name     string
		prefixes []netip.Prefix
	}{{"fake", state.FakeIP}, {"bypass", state.Bypass}} {
		for _, family := range []int{4, 6} {
			elements, err := nftPrefixElements(entry.prefixes, family)
			if err != nil {
				return nil, fmt.Errorf("transparent-proxy %s addresses: %w", entry.name, err)
			}
			kind := nftables.TypeIPAddr
			if family == 6 {
				kind = nftables.TypeIP6Addr
			}
			name := fmt.Sprintf("%s%d", entry.name, family)
			addressSets[name] = addSet(name, kind, elements, true)
		}
	}

	chains := make(map[string]*nftDesiredChain)
	for _, name := range []string{"prerouting", "output", "pre4", "pre6", "out4", "out6", "tproxy4", "tproxy6", "mark_tproxy4", "mark_tproxy6", "tun4", "tun6", "redir4", "redir6", "redir_nat_pre", "redir_nat_out", "redir_guard_pre", "redir_guard_out", "redir_check4", "redir_check6"} {
		chain := &nftables.Chain{Name: name, Table: ruleset.table}
		if name == "prerouting" || name == "output" {
			policy := nftables.ChainPolicyAccept
			chain.Type, chain.Policy, chain.Priority = nftables.ChainTypeFilter, &policy, nftables.ChainPriorityMangle
			chain.Hooknum = nftables.ChainHookPrerouting
			if name == "output" {
				chain.Type, chain.Hooknum = nftables.ChainTypeRoute, nftables.ChainHookOutput
			}
		}
		if name == "redir_nat_pre" || name == "redir_nat_out" || name == "redir_guard_pre" || name == "redir_guard_out" {
			policy := nftables.ChainPolicyAccept
			chain.Type, chain.Policy, chain.Priority = nftables.ChainTypeNAT, &policy, nftables.ChainPriorityNATDest
			chain.Hooknum = nftables.ChainHookPrerouting
			if strings.HasSuffix(name, "_out") {
				chain.Hooknum = nftables.ChainHookOutput
			}
			if strings.HasPrefix(name, "redir_guard_") {
				chain.Type = nftables.ChainTypeFilter
				chain.Priority = nftables.ChainPriorityRef(-90)
			}
		}
		desired := &nftDesiredChain{chain: chain}
		chains[name] = desired
		ruleset.chains = append(ruleset.chains, desired)
	}
	addRule := func(chain string, expressions ...expr.Any) {
		current := chains[chain]
		comment := fmt.Sprintf("%s/rule/%s/%d", nftModule, chain, len(current.rules))
		current.rules = append(current.rules, &nftables.Rule{
			Table: ruleset.table, Chain: current.chain, Exprs: expressions,
			UserData: userdata.AppendString(nil, userdata.TypeComment, comment),
		})
	}
	protocols := []struct {
		protocol byte
		mode     Mode
	}{{unix.IPPROTO_TCP, state.TCP}, {unix.IPPROTO_UDP, state.UDP}}
	buildNFTRedir(state, addRule)

	for _, family := range []int{4, 6} {
		if family == 6 && !state.IPv6 {
			continue
		}
		suffix := fmt.Sprint(family)
		pre, out := "pre"+suffix, "out"+suffix
		proxy, markProxy, tun := "tproxy"+suffix, "mark_tproxy"+suffix, "tun"+suffix
		nfproto := byte(unix.NFPROTO_IPV4)
		tcpReady, udpReady, tunReady := state.TProxyTCP4, state.TProxyUDP4, state.Tun4
		if family == 6 {
			nfproto = unix.NFPROTO_IPV6
			tcpReady, udpReady, tunReady = state.TProxyTCP6, state.TProxyUDP6, state.Tun6
		}
		addRule("prerouting", append(nftMetaMatch(expr.MetaKeyNFPROTO, []byte{nfproto}), nftGoto(pre))...)
		addRule("output", append(nftMetaMatch(expr.MetaKeyNFPROTO, []byte{nfproto}), nftGoto(out))...)

		// A failed transparent-socket lookup breaks its rule, not its chain.
		// Dedicated terminal chains therefore end with drop even when the
		// listener disappeared between a readiness event and packet delivery.
		for _, protocol := range protocols {
			ready := tcpReady
			if protocol.protocol == unix.IPPROTO_UDP {
				ready = udpReady
			}
			if protocol.mode != ModeTProxy || !ready {
				continue
			}
			addRule(proxy, append(nftProtocol(protocol.protocol),
				&expr.Immediate{Register: 1, Data: binaryutil.BigEndian.PutUint16(state.TProxyPort)},
				&expr.TProxy{Family: unix.NFPROTO_UNSPEC, RegPort: 1},
				&expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(state.TProxyMark)},
				&expr.Meta{Key: expr.MetaKeyMARK, SourceRegister: true, Register: 1}, nftAccept())...)
			addRule(markProxy, append(nftProtocol(protocol.protocol), nftSetMark(state.TProxyMark)...)...)
		}
		// Packet classification retains its mark even while the backend is
		// blocked. The terminal verdict still drops, and the actual working
		// rule remains sufficient to identify its policy after a restart.
		addRule(proxy, nftMarkVerdict(state.TProxyMark, nftDrop())...)
		addRule(markProxy, nftMarkVerdict(state.TProxyMark, nftDrop())...)
		if tunReady {
			addRule(tun, nftSetMark(state.TunMark)...)
		} else {
			addRule(tun, nftMarkVerdict(state.TunMark, nftDrop())...)
		}

		for _, chain := range []string{pre, out} {
			addRule(chain, &expr.Meta{Key: expr.MetaKeyMARK, Register: 1}, nftLookup(markSet, false), nftReturn())
		}
		addRule(out, &expr.Meta{Key: expr.MetaKeySKUID, Register: 1}, nftLookup(uidSet, false), nftReturn())
		// Listener replies retain the path of their original incoming flow.
		// A locally originated request is conntrack's original direction and
		// still reaches capture, including the later TProxy return through lo.
		addRule(out, &expr.Ct{Key: expr.CtKeyDIRECTION, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{1}}, nftReturn())
		if state.TunName != "" {
			addRule(pre, append(nftMetaMatch(expr.MetaKeyIIFNAME, nftInterfaceName(state.TunName)), nftReturn())...)
		}
		// Local TProxy OUTPUT packets return through lo, even if gateway
		// capture is disabled or restricted to another physical interface.
		for _, protocol := range protocols {
			if protocol.mode != ModeTProxy {
				continue
			}
			match := nftMetaMatch(expr.MetaKeyIIFNAME, nftInterfaceName("lo"))
			match = append(match, nftMetaMatch(expr.MetaKeyMARK, binaryutil.NativeEndian.PutUint32(state.TProxyMark))...)
			match = append(match, nftProtocol(protocol.protocol)...)
			addRule(pre, append(match, nftGoto(proxy))...)
		}
		if set := interfaces["gateway"]; set != nil {
			addRule(pre, &expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1}, nftLookup(set, true), nftReturn())
		}
		if set := interfaces["local"+suffix]; set != nil {
			addRule(out, &expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1}, nftLookup(set, true), nftReturn())
		}
		for _, protocol := range protocols {
			if protocol.mode != ModeTProxy {
				continue
			}
			ready := tcpReady
			if protocol.protocol == unix.IPPROTO_UDP {
				ready = udpReady
			}
			match := append(nftProtocol(protocol.protocol),
				&expr.Socket{Key: expr.SocketKeyTransparent, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{1}})
			if ready {
				addRule(pre, append(match, nftSetMark(state.TProxyMark)...)...)
			} else {
				addRule(pre, append(match, nftDrop())...)
			}
		}

		for _, entry := range []struct{ chain, proxy string }{{pre, proxy}, {out, markProxy}} {
			capture := func(mode Mode) expr.Any {
				if mode == ModeTProxy {
					return nftGoto(entry.proxy)
				}
				if mode == ModeRedir {
					return nftGoto("redir" + suffix)
				}
				return nftGoto(tun)
			}
			// FakeIP deliberately precedes FIB/prefix and local port bypass.
			// ModeOff has no matching rule here, including for fake addresses.
			for _, protocol := range protocols {
				if protocol.mode == ModeOff {
					continue
				}
				match := nftProtocol(protocol.protocol)
				match = append(match, nftDestination(family), nftLookup(addressSets["fake"+suffix], false), capture(protocol.mode))
				addRule(entry.chain, match...)
			}
			addRule(entry.chain, &expr.Fib{Register: 1, FlagDADDR: true, ResultADDRTYPE: true}, nftLookup(localTypeSet, false), nftReturn())
			addRule(entry.chain, nftDestination(family), nftLookup(addressSets["bypass"+suffix], false), nftReturn())
			if entry.chain == out {
				for _, protocol := range protocols {
					match := append(nftProtocol(protocol.protocol), &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2}, nftLookup(portSets[protocol.protocol], false), nftReturn())
					addRule(out, match...)
				}
			}
			for _, protocol := range protocols {
				if protocol.mode != ModeOff {
					addRule(entry.chain, append(nftProtocol(protocol.protocol), capture(protocol.mode))...)
				}
			}
		}
	}
	return ruleset, nil
}

func validNFTInterface(name string) bool {
	return name != "" && name != "*" && len(name) < unix.IFNAMSIZ && strings.IndexByte(name, 0) < 0
}

func nftInterfaceName(name string) []byte {
	value := make([]byte, unix.IFNAMSIZ)
	copy(value, name)
	return value
}

func nftUint32Elements(values []uint32) []nftables.SetElement {
	elements := make([]nftables.SetElement, 0, len(values))
	for _, value := range values {
		elements = append(elements, nftables.SetElement{Key: binaryutil.NativeEndian.PutUint32(value)})
	}
	return elements
}

func nftPrefixElements(prefixes []netip.Prefix, family int) ([]nftables.SetElement, error) {
	var builder netipx.IPSetBuilder
	for _, prefix := range prefixes {
		if !prefix.IsValid() {
			return nil, errors.New("invalid IP prefix")
		}
		if prefix.Addr().Is4() == (family == 4) {
			builder.AddPrefix(prefix.Masked())
		}
	}
	set, err := builder.IPSet()
	if err != nil {
		return nil, err
	}
	var elements []nftables.SetElement
	for _, addressRange := range set.Ranges() {
		end := addressRange.To().Next()
		if !end.IsValid() {
			end = netip.IPv6Unspecified()
			if family == 4 {
				end = netip.IPv4Unspecified()
			}
		}
		elements = append(elements, nftables.SetElement{Key: addressRange.From().AsSlice()}, nftables.SetElement{Key: end.AsSlice(), IntervalEnd: true})
	}
	return normalizeNFTElements(elements), nil
}

func normalizeNFTElements(elements []nftables.SetElement) []nftables.SetElement {
	if len(elements) == 0 {
		return nil
	}
	result := append([]nftables.SetElement(nil), elements...)
	sort.Slice(result, func(i, j int) bool {
		if order := bytes.Compare(result[i].Key, result[j].Key); order != 0 {
			return order < 0
		}
		return result[i].IntervalEnd && !result[j].IntervalEnd
	})
	unique := result[:0]
	for _, element := range result {
		if len(unique) == 0 || !bytes.Equal(unique[len(unique)-1].Key, element.Key) || unique[len(unique)-1].IntervalEnd != element.IntervalEnd {
			unique = append(unique, element)
		}
	}
	return unique
}

func nftMetaMatch(key expr.MetaKey, value []byte) []expr.Any {
	return []expr.Any{&expr.Meta{Key: key, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: value}}
}
func nftProtocol(protocol byte) []expr.Any {
	return nftMetaMatch(expr.MetaKeyL4PROTO, []byte{protocol})
}
func nftLookup(set *nftables.Set, invert bool) *expr.Lookup {
	return &expr.Lookup{SourceRegister: 1, SetID: set.ID, SetName: set.Name, Invert: invert}
}
func nftDestination(family int) *expr.Payload {
	if family == 6 {
		return &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 24, Len: 16}
	}
	return &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4}
}
func nftGoto(chain string) *expr.Verdict { return &expr.Verdict{Kind: expr.VerdictGoto, Chain: chain} }
func nftReturn() *expr.Verdict           { return &expr.Verdict{Kind: expr.VerdictReturn} }
func nftAccept() *expr.Verdict           { return &expr.Verdict{Kind: expr.VerdictAccept} }
func nftDrop() *expr.Verdict             { return &expr.Verdict{Kind: expr.VerdictDrop} }
func nftSetMark(mark uint32) []expr.Any {
	return nftMarkVerdict(mark, nftAccept())
}

func nftMarkVerdict(mark uint32, verdict *expr.Verdict) []expr.Any {
	return []expr.Any{
		&expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(mark)},
		&expr.Meta{Key: expr.MetaKeyMARK, SourceRegister: true, Register: 1}, verdict,
	}
}

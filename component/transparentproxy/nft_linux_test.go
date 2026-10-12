//go:build linux

package transparentproxy

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/metacubex/nftables"
	"github.com/metacubex/nftables/binaryutil"
	"github.com/metacubex/nftables/expr"
	"github.com/metacubex/nftables/userdata"
	"golang.org/x/sys/unix"
)

func nftTestState() firewallState {
	return firewallState{
		Enabled: true, TCP: ModeTProxy, UDP: ModeTUN, IPv6: true,
		TProxyMark: 1, TunMark: 0x162, TProxyPort: 7895, TunName: "utun",
		TProxyTCP4: true, TProxyTCP6: true, TProxyUDP4: true, TProxyUDP6: true, Tun4: true, Tun6: true,
		Local4: []string{"eth0"}, Local6: []string{"eth0"}, Gateway: []string{"eth0"},
		BypassUIDs: []uint32{1001}, BypassMarks: []uint32{0x9001}, BypassUDPPorts: []uint16{123},
		FakeIP: []netip.Prefix{netip.MustParsePrefix("198.18.0.0/16"), netip.MustParsePrefix("fdfe:dcba:9876::/64")},
		Bypass: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("240.0.0.0/4")},
	}
}

type nftTestPacket struct {
	address                             netip.Addr
	protocol                            byte
	input, output                       string
	uid, mark                           uint32
	port                                uint16
	fibType                             uint32
	reply, transparent, missingListener bool
	tproxied                            bool
}

// This small packet evaluator executes the generated expression objects. It
// tests packet decisions through their serialized-program boundary instead of
// repeating the builder's classification logic. Kernel round-trip tests below
// separately verify that Linux accepts and reads back the same program.
func evaluateNFT(t *testing.T, program *nftRuleset, hook string, packet nftTestPacket) (nftTestPacket, expr.VerdictKind) {
	t.Helper()
	chains := make(map[string]*nftDesiredChain)
	sets := make(map[string]*nftDesiredSet)
	for _, chain := range program.chains {
		chains[chain.chain.Name] = chain
	}
	for _, set := range program.sets {
		sets[set.set.Name] = set
	}
	registers := make(map[uint32][]byte)
	var evaluate func(string, int) expr.VerdictKind
	evaluate = func(name string, depth int) expr.VerdictKind {
		if depth > 20 || chains[name] == nil {
			t.Fatalf("invalid generated chain traversal %q", name)
		}
	rules:
		for _, rule := range chains[name].rules {
			for _, expression := range rule.Exprs {
				switch value := expression.(type) {
				case *expr.Meta:
					if value.SourceRegister {
						if value.Key != expr.MetaKeyMARK {
							t.Fatal("unexpected writable metadata")
						}
						packet.mark = binaryutil.NativeEndian.Uint32(registers[value.Register])
						continue
					}
					var data []byte
					switch value.Key {
					case expr.MetaKeyNFPROTO:
						data = []byte{unix.NFPROTO_IPV6}
						if packet.address.Is4() {
							data[0] = unix.NFPROTO_IPV4
						}
					case expr.MetaKeyL4PROTO:
						data = []byte{packet.protocol}
					case expr.MetaKeyIIFNAME:
						data = nftInterfaceName(packet.input)
					case expr.MetaKeyOIFNAME:
						data = nftInterfaceName(packet.output)
					case expr.MetaKeyMARK:
						data = binaryutil.NativeEndian.PutUint32(packet.mark)
					case expr.MetaKeySKUID:
						data = binaryutil.NativeEndian.PutUint32(packet.uid)
					default:
						t.Fatalf("unsupported metadata %v", value.Key)
					}
					registers[value.Register] = data
				case *expr.Cmp:
					if value.Op != expr.CmpOpEq {
						t.Fatal("unsupported comparison")
					}
					if !bytes.Equal(registers[value.Register], value.Data) {
						continue rules
					}
				case *expr.Immediate:
					registers[value.Register] = value.Data
				case *expr.Payload:
					if value.Base == expr.PayloadBaseNetworkHeader {
						if packet.address.Is4() && (value.Offset != 16 || value.Len != 4) || packet.address.Is6() && (value.Offset != 24 || value.Len != 16) {
							t.Fatal("wrong destination address layout")
						}
						registers[value.DestRegister] = packet.address.AsSlice()
					} else if value.Base == expr.PayloadBaseTransportHeader && value.Offset == 2 && value.Len == 2 {
						registers[value.DestRegister] = binaryutil.BigEndian.PutUint16(packet.port)
					} else {
						t.Fatal("unexpected payload read")
					}
				case *expr.Lookup:
					set := sets[value.SetName]
					if set == nil {
						t.Fatalf("unknown set %q", value.SetName)
					}
					matched := false
					key := registers[value.SourceRegister]
					for _, element := range set.elements {
						if set.set.Interval {
							if bytes.Compare(element.Key, key) <= 0 {
								matched = !element.IntervalEnd
							}
						} else if bytes.Equal(element.Key, key) {
							matched = true
						}
					}
					if matched == value.Invert {
						continue rules
					}
				case *expr.Fib:
					if !value.FlagDADDR || !value.ResultADDRTYPE || value.FlagMARK {
						t.Fatal("unexpected FIB query")
					}
					registers[value.Register] = binaryutil.NativeEndian.PutUint32(packet.fibType)
				case *expr.Ct:
					if value.Key != expr.CtKeyDIRECTION {
						t.Fatal("unexpected conntrack read")
					}
					registers[value.Register] = []byte{0}
					if packet.reply {
						registers[value.Register][0] = 1
					}
				case *expr.Socket:
					if !packet.transparent {
						continue rules
					}
					registers[value.Register] = []byte{1}
				case *expr.TProxy:
					if packet.missingListener {
						continue rules
					}
					if binaryutil.BigEndian.Uint16(registers[value.RegPort]) != 7895 {
						t.Fatal("incorrect TProxy listener port")
					}
					packet.tproxied = true
				case *expr.Verdict:
					if value.Kind == expr.VerdictGoto {
						return evaluate(value.Chain, depth+1)
					}
					return value.Kind
				default:
					t.Fatalf("unexpected expression %T", expression)
				}
			}
		}
		return expr.VerdictReturn
	}
	verdict := evaluate(hook, 0)
	if verdict == expr.VerdictReturn {
		verdict = expr.VerdictAccept
	}
	return packet, verdict
}

func TestNFTProtocolModesAndAddressFamilies(t *testing.T) {
	for _, tcp := range []Mode{ModeOff, ModeTProxy, ModeTUN} {
		for _, udp := range []Mode{ModeOff, ModeTProxy, ModeTUN} {
			t.Run(string(tcp)+"-"+string(udp), func(t *testing.T) {
				state := nftTestState()
				state.TCP, state.UDP = tcp, udp
				program, err := buildNFTRuleset("test-owner", state)
				if err != nil {
					t.Fatal(err)
				}
				for _, address := range []string{"8.8.8.8", "2001:4860:4860::8888", "198.18.0.2", "fdfe:dcba:9876::2"} {
					for _, hook := range []string{"output", "prerouting"} {
						for _, protocol := range []byte{unix.IPPROTO_TCP, unix.IPPROTO_UDP} {
							packet := nftTestPacket{address: netip.MustParseAddr(address), protocol: protocol, input: "eth0", output: "eth0", port: 80, fibType: unix.RTN_UNICAST}
							result, verdict := evaluateNFT(t, program, hook, packet)
							mode := tcp
							if protocol == unix.IPPROTO_UDP {
								mode = udp
							}
							wantMark := uint32(0)
							if mode == ModeTProxy {
								wantMark = state.TProxyMark
							}
							if mode == ModeTUN {
								wantMark = state.TunMark
							}
							if result.mark != wantMark || verdict != expr.VerdictAccept || result.tproxied != (mode == ModeTProxy && hook == "prerouting") {
								t.Fatalf("%s %s protocol=%d: result=%+v verdict=%v mode=%s", hook, address, protocol, result, verdict, mode)
							}
						}
					}
				}
			})
		}
	}
}

func TestNFTCaptureOrderingAndReadiness(t *testing.T) {
	tests := []struct {
		name, hook, address string
		protocol            byte
		changeState         func(*firewallState)
		changePacket        func(*nftTestPacket)
		mark                uint32
		drop, tproxy        bool
	}{
		{name: "local-interface", hook: "output", address: "8.8.8.8", protocol: unix.IPPROTO_TCP, changePacket: func(p *nftTestPacket) { p.output = "eth1" }},
		{name: "gateway-interface", hook: "prerouting", address: "8.8.8.8", protocol: unix.IPPROTO_TCP, changePacket: func(p *nftTestPacket) { p.input = "eth1" }},
		{name: "gateway-off", hook: "prerouting", address: "8.8.8.8", protocol: unix.IPPROTO_UDP, changeState: func(s *firewallState) { s.Gateway = nil }},
		{name: "local-off", hook: "output", address: "8.8.8.8", protocol: unix.IPPROTO_UDP, changeState: func(s *firewallState) { s.Local4 = nil }},
		{name: "wildcard-gateway", hook: "prerouting", address: "8.8.8.8", protocol: unix.IPPROTO_UDP, changeState: func(s *firewallState) { s.Gateway = []string{"*"} }, changePacket: func(p *nftTestPacket) { p.input = "lan1" }, mark: 0x162},
		{name: "tun-input-loop", hook: "prerouting", address: "198.18.0.2", protocol: unix.IPPROTO_UDP, changePacket: func(p *nftTestPacket) { p.input = "utun" }},
		{name: "core-mark-before-fake", hook: "output", address: "198.18.0.2", protocol: unix.IPPROTO_TCP, changePacket: func(p *nftTestPacket) { p.mark = 0x9001 }, mark: 0x9001},
		{name: "uid-before-fake", hook: "output", address: "198.18.0.2", protocol: unix.IPPROTO_TCP, changePacket: func(p *nftTestPacket) { p.uid = 1001 }},
		{name: "root-not-implicitly-bypassed", hook: "output", address: "8.8.8.8", protocol: unix.IPPROTO_TCP, mark: 1},
		{name: "reply-before-fake", hook: "output", address: "198.18.0.2", protocol: unix.IPPROTO_TCP, changePacket: func(p *nftTestPacket) { p.reply = true }},
		{name: "reply-bypass-only-output", hook: "prerouting", address: "8.8.8.8", protocol: unix.IPPROTO_UDP, changePacket: func(p *nftTestPacket) { p.reply = true }, mark: 0x162},
		{name: "private", hook: "output", address: "10.5.6.7", protocol: unix.IPPROTO_TCP},
		{name: "highest-reserved-ip", hook: "output", address: "255.255.255.255", protocol: unix.IPPROTO_TCP},
		{name: "fib-local", hook: "prerouting", address: "8.8.8.8", protocol: unix.IPPROTO_TCP, changePacket: func(p *nftTestPacket) { p.fibType = unix.RTN_LOCAL }},
		{name: "fake-before-fib", hook: "prerouting", address: "198.18.0.2", protocol: unix.IPPROTO_TCP, changePacket: func(p *nftTestPacket) { p.fibType = unix.RTN_LOCAL }, mark: 1, tproxy: true},
		{name: "fake-before-private", hook: "output", address: "fdfe:dcba:9876::2", protocol: unix.IPPROTO_UDP, mark: 0x162},
		{name: "local-ntp", hook: "output", address: "8.8.8.8", protocol: unix.IPPROTO_UDP, changePacket: func(p *nftTestPacket) { p.port = 123 }},
		{name: "gateway-ntp", hook: "prerouting", address: "8.8.8.8", protocol: unix.IPPROTO_UDP, changePacket: func(p *nftTestPacket) { p.port = 123 }, mark: 0x162},
		{name: "fake-before-ntp", hook: "output", address: "198.18.0.2", protocol: unix.IPPROTO_UDP, changePacket: func(p *nftTestPacket) { p.port = 123 }, mark: 0x162},
		{name: "off-includes-fake", hook: "output", address: "198.18.0.2", protocol: unix.IPPROTO_UDP, changeState: func(s *firewallState) { s.UDP = ModeOff }},
		{name: "ipv6-disabled", hook: "output", address: "fdfe:dcba:9876::2", protocol: unix.IPPROTO_UDP, changeState: func(s *firewallState) { s.IPv6 = false }},
		{name: "tun4-unready", hook: "output", address: "8.8.8.8", protocol: unix.IPPROTO_UDP, changeState: func(s *firewallState) { s.Tun4 = false }, drop: true},
		{name: "tun4-unready-keeps-v6", hook: "output", address: "2001:4860:4860::8888", protocol: unix.IPPROTO_UDP, changeState: func(s *firewallState) { s.Tun4 = false }, mark: 0x162},
		{name: "tun-unready-bypass", hook: "output", address: "10.1.2.3", protocol: unix.IPPROTO_UDP, changeState: func(s *firewallState) { s.Tun4 = false }},
		{name: "tproxy-tcp-unready", hook: "prerouting", address: "8.8.8.8", protocol: unix.IPPROTO_TCP, changeState: func(s *firewallState) { s.TProxyTCP4 = false }, drop: true},
		{name: "tproxy-udp-still-ready", hook: "prerouting", address: "8.8.8.8", protocol: unix.IPPROTO_UDP, changeState: func(s *firewallState) { s.TProxyTCP4 = false; s.UDP = ModeTProxy }, mark: 1, tproxy: true},
		{name: "missing-tproxy-socket-drops", hook: "prerouting", address: "8.8.8.8", protocol: unix.IPPROTO_TCP, changePacket: func(p *nftTestPacket) { p.missingListener = true }, drop: true},
		{name: "lo-return-with-gateway-off", hook: "prerouting", address: "10.1.2.3", protocol: unix.IPPROTO_TCP, changeState: func(s *firewallState) { s.Gateway = nil }, changePacket: func(p *nftTestPacket) { p.input = "lo"; p.mark = 1 }, mark: 1, tproxy: true},
		{name: "lo-return-unready", hook: "prerouting", address: "10.1.2.3", protocol: unix.IPPROTO_TCP, changeState: func(s *firewallState) { s.Gateway = nil; s.TProxyTCP4 = false }, changePacket: func(p *nftTestPacket) { p.input = "lo"; p.mark = 1 }, mark: 1, drop: true},
		{name: "transparent-existing-socket", hook: "prerouting", address: "10.1.2.3", protocol: unix.IPPROTO_TCP, changePacket: func(p *nftTestPacket) { p.transparent = true }, mark: 1},
		{name: "transparent-existing-unready", hook: "prerouting", address: "10.1.2.3", protocol: unix.IPPROTO_TCP, changeState: func(s *firewallState) { s.TProxyTCP4 = false }, changePacket: func(p *nftTestPacket) { p.transparent = true }, drop: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := nftTestState()
			if test.changeState != nil {
				test.changeState(&state)
			}
			program, err := buildNFTRuleset("test-owner", state)
			if err != nil {
				t.Fatal(err)
			}
			packet := nftTestPacket{address: netip.MustParseAddr(test.address), protocol: test.protocol, input: "eth0", output: "eth0", port: 80, fibType: unix.RTN_UNICAST}
			if test.changePacket != nil {
				test.changePacket(&packet)
			}
			result, verdict := evaluateNFT(t, program, test.hook, packet)
			if result.mark != test.mark || (verdict == expr.VerdictDrop) != test.drop || result.tproxied != test.tproxy {
				t.Fatalf("result=%+v verdict=%v; want mark=%#x drop=%v tproxy=%v", result, verdict, test.mark, test.drop, test.tproxy)
			}
		})
	}
}

func nftSnapshotFromProgram(t *testing.T, program *nftRuleset) *nftSnapshot {
	t.Helper()
	snapshot := &nftSnapshot{generation: 1, table: program.table, chains: make(map[string]*nftChainState), sets: make(map[string]*nftSetState)}
	for _, set := range program.sets {
		flags := uint32(0)
		if set.set.Interval {
			flags = unix.NFT_SET_INTERVAL
		}
		copySet := *set.set
		snapshot.sets[set.set.Name] = &nftSetState{set: &copySet, flags: flags, elements: append([]nftables.SetElement(nil), set.elements...)}
	}
	handle := uint64(1)
	for _, chain := range program.chains {
		copyChain := *chain.chain
		current := &nftChainState{chain: &copyChain}
		for _, rule := range chain.rules {
			copyRule := *rule
			copyRule.Handle = handle
			handle++
			fingerprint, err := nftRuleFingerprint(rule)
			if err != nil {
				t.Fatal(err)
			}
			comment, err := nftUserdataString(rule.UserData, byte(userdata.TypeComment))
			if err != nil {
				t.Fatal(err)
			}
			current.rules = append(current.rules, &nftRuleState{rule: &copyRule, comment: comment, expressions: fingerprint})
		}
		snapshot.chains[chain.chain.Name] = current
	}
	return snapshot
}

func TestNFTReconcilePreservesOwnershipAndConverges(t *testing.T) {
	state := nftTestState()
	program, err := buildNFTRuleset("stable-owner", state)
	if err != nil {
		t.Fatal(err)
	}
	actual := nftSnapshotFromProgram(t, program)
	plan, err := planNFTChanges(actual, program)
	if err != nil || !plan.empty() {
		t.Fatalf("own event did not converge: %+v %v", plan, err)
	}

	state.Gateway = []string{"eth0", "eth0"}
	state.Bypass = append(state.Bypass, netip.MustParsePrefix("10.1.0.0/16"))
	reordered, err := buildNFTRuleset("stable-owner", state)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = planNFTChanges(actual, reordered)
	if err != nil || !plan.empty() {
		t.Fatalf("equivalent normalized input rewrites rules: %+v %v", plan, err)
	}
	state.Bypass = append(state.Bypass, netip.MustParsePrefix("2001:db8:1234::/56"))
	changed, err := buildNFTRuleset("stable-owner", state)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = planNFTChanges(actual, changed)
	if err != nil || len(plan.updateSets) != 1 || plan.updateSets[0].set.Name != "bypass6" || len(plan.deleteRules)+len(plan.createRules)+len(plan.deleteChains)+len(plan.createChains) != 0 {
		t.Fatalf("prefix update rewrote unrelated rules: %+v %v", plan, err)
	}

	actual = nftSnapshotFromProgram(t, program)
	actual.chains["pre4"].rules = nil
	plan, err = planNFTChanges(actual, program)
	if err != nil || len(plan.createRules) != len(program.chains[2].rules) || len(plan.createSets) != 0 {
		t.Fatalf("flushed chain not repaired locally: %+v %v", plan, err)
	}
	actual = &nftSnapshot{chains: make(map[string]*nftChainState), sets: make(map[string]*nftSetState)}
	plan, err = planNFTChanges(actual, program)
	if err != nil || plan.createTable == nil || len(plan.createChains) != len(program.chains) || len(plan.createSets) != len(program.sets) {
		t.Fatalf("deleted table not recreated: %+v %v", plan, err)
	}

	actual.table = program.table
	if _, err := planNFTChanges(actual, program); err == nil {
		t.Fatal("adopted unknown same-name table")
	}
	actual = nftSnapshotFromProgram(t, program)
	actual.sets["fake4"].set.Comment = "administrator"
	if _, err := planNFTChanges(actual, program); err == nil {
		t.Fatal("replaced foreign same-name set")
	}
	actual = nftSnapshotFromProgram(t, program)
	actual.chains["pre4"].rules[0].comment = "administrator"
	if _, err := planNFTChanges(actual, program); err == nil {
		t.Fatal("removed foreign rule inside managed chain")
	}
	actual = nftSnapshotFromProgram(t, program)
	delete(actual.sets, "owner")
	plan, err = planNFTChanges(actual, program)
	if err != nil || len(plan.createSets) != 1 || plan.createSets[0].set.Name != "owner" {
		t.Fatalf("intact owned objects could not recover marker: %+v %v", plan, err)
	}
}

func TestNFTWireExpressionFingerprint(t *testing.T) {
	for _, expression := range []expr.Any{
		&expr.TProxy{Family: unix.NFPROTO_UNSPEC, RegPort: 1},
		&expr.Socket{Key: expr.SocketKeyTransparent, Register: 1},
		&expr.Lookup{SourceRegister: 1, SetName: "fake4", SetID: 99},
		nftGoto("pre4"),
	} {
		data, err := expr.Marshal(byte(nftables.TableFamilyINet), expression)
		if err != nil {
			t.Fatal(err)
		}
		wanted, err := canonicalNFTExpression(data)
		if err != nil {
			t.Fatal(err)
		}
		outer, err := nftAttributeMap(data)
		if err != nil {
			t.Fatal(err)
		}
		fields, err := parseWireAttributes(outer[unix.NFTA_EXPR_DATA])
		if err != nil {
			t.Fatal(err)
		}
		var changed []byte
		for i := len(fields) - 1; i >= 0; i-- {
			field := fields[i]
			if _, ok := expression.(*expr.Lookup); ok && field.Type&0x3fff == unix.NFTA_LOOKUP_SET_ID {
				continue
			}
			if _, ok := expression.(*expr.Socket); ok && field.Type&0x3fff == expr.NFTA_SOCKET_LEVEL {
				continue
			}
			if _, ok := expression.(*expr.Verdict); ok && field.Type&0x3fff == unix.NFTA_IMMEDIATE_DATA {
				data, err := nftAttributeMap(field.Data)
				if err != nil {
					t.Fatal(err)
				}
				field.Data = marshalNFTAttributes(data)
			}
			changed = appendWireAttribute(changed, field.Type, field.Data)
		}
		response := appendWireAttribute(nil, unix.NFTA_EXPR_DATA, changed)
		response = appendWireAttribute(response, unix.NFTA_EXPR_NAME, outer[unix.NFTA_EXPR_NAME])
		got, err := canonicalNFTExpression(response)
		if err != nil || !bytes.Equal(got, wanted) {
			t.Fatalf("%T does not round-trip canonically: %v", expression, err)
		}
		if _, ok := expression.(*expr.TProxy); ok {
			changed := &expr.TProxy{Family: unix.NFPROTO_UNSPEC, RegPort: 2}
			other, _ := expr.Marshal(byte(nftables.TableFamilyINet), changed)
			fingerprint, _ := canonicalNFTExpression(other)
			if bytes.Equal(fingerprint, wanted) {
				t.Fatal("TProxy register change was lost")
			}
		}
	}
	for _, malformed := range [][]byte{{1}, {0, 255}, {0, 2, 'x'}} {
		if _, err := nftUserdataString(malformed, 0); err == nil {
			t.Fatal("accepted malformed owner comment")
		}
	}
	if _, err := decodeNFTRule(wireMessage{Type: unix.NFT_MSG_NEWRULE, Data: []byte{1}}); err == nil {
		t.Fatal("accepted truncated rule")
	}
	padded := appendWireAttribute(nil, unix.NFTA_RULE_PAD, nil)
	padded = appendWireAttribute(padded, unix.NFTA_RULE_PAD, nil)
	if _, err := nftAttributeMap(padded); err != nil {
		t.Fatalf("rejected valid kernel alignment padding: %v", err)
	}
}

func TestNFTApplyFailureAndCloseDoNotDeleteForeignObjects(t *testing.T) {
	state := nftTestState()
	program, err := buildNFTRuleset("stable-owner", state)
	if err != nil {
		t.Fatal(err)
	}
	actual := nftSnapshotFromProgram(t, program)
	foreign := &nftRuleState{rule: &nftables.Rule{Table: program.table, Chain: &nftables.Chain{Name: "administrator", Table: program.table}, Handle: 999}, comment: "administrator"}
	actual.chains["administrator"] = &nftChainState{chain: foreign.rule.Chain, rules: []*nftRuleState{foreign}}
	var mutations []wireMessage
	write := func(_ context.Context, _ uint32, messages []wireMessage) error {
		mutations = append(mutations, messages...)
		return unix.EPERM
	}
	f := &nftFirewall{owner: "stable-owner", read: func(context.Context, string) (*nftSnapshot, error) { return actual, nil }, write: write}
	if err := f.Apply(context.Background(), state); err != nil || len(mutations) != 0 {
		t.Fatalf("unchanged Apply sent transaction: %v", err)
	}
	state.Tun4 = false
	if err := f.Apply(context.Background(), state); !errors.Is(err, unix.EPERM) {
		t.Fatalf("write failure hidden: %v", err)
	}
	mutations = nil
	if err := f.Close(context.Background()); !errors.Is(err, unix.EPERM) {
		t.Fatalf("close failure hidden: %v", err)
	}
	for _, message := range mutations {
		if message.Type != uint16(unix.NFNL_SUBSYS_NFTABLES<<8)|unix.NFT_MSG_DELRULE {
			t.Fatalf("Close removed a table/chain/set: %v", message.Type)
		}
		attributes, err := nftAttributeMap(message.Data[4:])
		if err != nil {
			t.Fatal(err)
		}
		chain, err := nftStringAttribute(attributes, unix.NFTA_RULE_CHAIN)
		if err != nil {
			t.Fatal(err)
		}
		if chain == "administrator" {
			t.Fatal("Close removed a foreign rule")
		}
		if len(attributes[unix.NFTA_RULE_HANDLE]) != 8 {
			t.Fatal("Close issued unscoped rule flush")
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	mutations = nil
	if err := f.Apply(cancelled, state); !errors.Is(err, context.Canceled) || len(mutations) != 0 {
		t.Fatalf("cancelled apply wrote mutations: %v", err)
	}
	actual = &nftSnapshot{table: program.table, chains: map[string]*nftChainState{}, sets: map[string]*nftSetState{}}
	if err := f.Close(context.Background()); err == nil || !strings.Contains(err.Error(), "not owned") || len(mutations) != 0 {
		t.Fatalf("Close claimed foreign table: %v", err)
	}
}

func TestNFTInvalidStateIsRejectedBeforeCapture(t *testing.T) {
	for _, change := range []func(*firewallState){
		func(s *firewallState) { s.TCP = "invalid" },
		func(s *firewallState) { s.TProxyMark = 0 },
		func(s *firewallState) { s.TunMark = s.TProxyMark },
		func(s *firewallState) { s.BypassMarks = append(s.BypassMarks, s.TunMark) },
		func(s *firewallState) { s.TProxyPort = 0 },
		func(s *firewallState) { s.TunName = "" },
		func(s *firewallState) { s.Local4 = []string{"this-interface-name-is-too-long"} },
		func(s *firewallState) { s.Bypass = []netip.Prefix{{}} },
	} {
		state := nftTestState()
		change(&state)
		if _, err := buildNFTRuleset("test-owner", state); err == nil {
			t.Fatalf("accepted invalid state: %+v", state)
		}
	}
	if _, err := newFirewall(""); err == nil {
		t.Fatal("accepted empty owner")
	}
}

//go:build linux

package transparentproxy

import (
	"net/netip"
	"testing"

	"github.com/metacubex/nftables/expr"
	"golang.org/x/sys/unix"
)

func TestNFTRedirCaptureAndGuardAcrossFamilies(t *testing.T) {
	for _, address := range []string{"8.8.8.8", "2001:4860:4860::8888", "198.18.0.2", "fdfe:dcba:9876::2"} {
		for _, hook := range []string{"output", "prerouting"} {
			t.Run(hook+"/"+address, func(t *testing.T) {
				state := nftTestState()
				state.TCP = ModeRedir
				program, err := buildNFTRuleset(state)
				if err != nil {
					t.Fatal(err)
				}
				packet := nftTestPacket{address: netip.MustParseAddr(address), protocol: unix.IPPROTO_TCP, input: "eth0", output: "eth0", port: 443, fibType: unix.RTN_UNICAST}
				packet, verdict := evaluateNFT(t, program, hook, packet)
				if verdict != expr.VerdictAccept || packet.mark != RedirMark {
					t.Fatalf("capture failed: %+v %v", packet, verdict)
				}
				path := "pre"
				if hook == "output" {
					path = "out"
				}
				packet, verdict = evaluateNFT(t, program, "redir_nat_"+path, packet)
				if verdict != expr.VerdictAccept || !packet.redirected || packet.ctMark != RedirMark || packet.port != state.RedirPort {
					t.Fatalf("REDIRECT did not establish its own NAT session: %+v", packet)
				}
				_, verdict = evaluateNFT(t, program, "redir_guard_"+path, packet)
				if verdict != expr.VerdictAccept {
					t.Fatal("valid locally translated session was dropped")
				}
			})
		}
	}
}

func TestNFTRedirEstablishedSessionsCannotSkipProtection(t *testing.T) {
	for _, test := range []struct {
		name         string
		changeState  func(*firewallState)
		changePacket func(*nftTestPacket)
		runNAT       bool
		wantDrop     bool
		wantCT       uint32
	}{
		{name: "new-session", runNAT: true, wantCT: RedirMark},
		{name: "established-direct-session", wantDrop: true},
		{name: "NAT-chain-disappeared", wantDrop: true},
		{name: "foreign-conntrack-mark", runNAT: true, changePacket: func(p *nftTestPacket) { p.ctMark = 0x9002 }, wantDrop: true, wantCT: 0x9002},
		{name: "foreign-DNAT-is-not-ours", runNAT: true, changePacket: func(p *nftTestPacket) { p.ctStatus = 1 << 5 }, wantDrop: true},
		{name: "current-established-NAT", changePacket: func(p *nftTestPacket) {
			p.ctMark, p.ctStatus, p.port, p.fibType = RedirMark, 1<<5, 7892, unix.RTN_LOCAL
		}, wantCT: RedirMark},
		{name: "old-listener-port", changePacket: func(p *nftTestPacket) {
			p.ctMark, p.ctStatus, p.port, p.fibType = RedirMark, 1<<5, 7891, unix.RTN_LOCAL
		}, wantDrop: true, wantCT: RedirMark},
		{name: "translated-address-is-no-longer-local", changePacket: func(p *nftTestPacket) { p.ctMark, p.ctStatus, p.port = RedirMark, 1<<5, 7892 }, wantDrop: true, wantCT: RedirMark},
		{name: "unavailable-listener", changeState: func(s *firewallState) { s.Redir4, s.Redir6 = false, false }, changePacket: func(p *nftTestPacket) {
			p.ctMark, p.ctStatus, p.port, p.fibType = RedirMark, 1<<5, 7892, unix.RTN_LOCAL
		}, wantDrop: true, wantCT: RedirMark},
		{name: "mode-changed-to-off", changeState: func(s *firewallState) { s.TCP = ModeOff }, changePacket: func(p *nftTestPacket) {
			p.ctMark, p.ctStatus, p.port, p.fibType, p.mark = RedirMark, 1<<5, 7892, unix.RTN_LOCAL, 0
		}, wantDrop: true, wantCT: RedirMark},
		{name: "mode-changed-to-tun", changeState: func(s *firewallState) { s.TCP = ModeTUN }, changePacket: func(p *nftTestPacket) {
			p.ctMark, p.ctStatus, p.port, p.fibType, p.mark = RedirMark, 1<<5, 7892, unix.RTN_LOCAL, 0x162
		}, wantDrop: true, wantCT: RedirMark},
		{name: "accepted-socket-reply", changePacket: func(p *nftTestPacket) { p.ctMark, p.mark, p.reply = RedirMark, DefaultOutboundMark, true }, wantCT: RedirMark},
	} {
		for _, address := range []string{"8.8.8.8", "2001:4860:4860::8888"} {
			for _, path := range []string{"pre", "out"} {
				t.Run(test.name+"/"+path+"/"+address, func(t *testing.T) {
					state := nftTestState()
					state.TCP = ModeRedir
					if test.changeState != nil {
						test.changeState(&state)
					}
					program, err := buildNFTRuleset(state)
					if err != nil {
						t.Fatal(err)
					}
					packet := nftTestPacket{address: netip.MustParseAddr(address), protocol: unix.IPPROTO_TCP, port: 443, mark: RedirMark, fibType: unix.RTN_UNICAST}
					if test.changePacket != nil {
						test.changePacket(&packet)
					}
					if test.runNAT {
						packet, _ = evaluateNFT(t, program, "redir_nat_"+path, packet)
					}
					result, verdict := evaluateNFT(t, program, "redir_guard_"+path, packet)
					if (verdict == expr.VerdictDrop) != test.wantDrop || result.ctMark != test.wantCT {
						t.Fatalf("unsafe NAT session decision: %+v verdict=%v wantDrop=%v wantCT=%x", result, verdict, test.wantDrop, test.wantCT)
					}
				})
			}
		}
	}
}

func TestNFTTCPPortBypassOnlyAppliesToLocalOrdinaryDestinations(t *testing.T) {
	for _, mode := range []Mode{ModeTProxy, ModeTUN, ModeRedir} {
		for _, test := range []struct {
			name, hook, address string
			bypass              bool
		}{
			{"local", "output", "8.8.8.8", true},
			{"local-v6", "output", "2001:4860:4860::8888", true},
			{"gateway", "prerouting", "8.8.8.8", false},
			{"gateway-v6", "prerouting", "2001:4860:4860::8888", false},
			{"fake-local", "output", "198.18.0.2", false},
			{"fake-local-v6", "output", "fdfe:dcba:9876::2", false},
		} {
			t.Run(string(mode)+"/"+test.name, func(t *testing.T) {
				state := nftTestState()
				state.TCP, state.BypassTCPPorts = mode, []uint16{443}
				program, err := buildNFTRuleset(state)
				if err != nil {
					t.Fatal(err)
				}
				packet := nftTestPacket{address: netip.MustParseAddr(test.address), protocol: unix.IPPROTO_TCP, input: "eth0", output: "eth0", port: 443, fibType: unix.RTN_UNICAST}
				result, verdict := evaluateNFT(t, program, test.hook, packet)
				if verdict != expr.VerdictAccept || (result.mark == 0) != test.bypass {
					t.Fatalf("TCP port bypass changed scope or overrode Fake-IP: %+v %v", result, verdict)
				}
				packet.protocol = unix.IPPROTO_UDP
				result, _ = evaluateNFT(t, program, test.hook, packet)
				if result.mark != state.TunMark {
					t.Fatalf("TCP port list incorrectly bypassed UDP: %+v", result)
				}
			})
		}
	}
}

func TestNFTRedirOwnershipRecoveryAndNormalStop(t *testing.T) {
	state := nftTestState()
	state.TCP, state.UDP = ModeRedir, ModeOff
	program, err := buildNFTRuleset(state)
	if err != nil {
		t.Fatal(err)
	}
	actual := nftSnapshotFromProgram(t, program)
	scopes, err := recoverNFTScopes(actual)
	if err != nil || len(scopes) != 0 || !hasNFTModuleObjects(actual) {
		t.Fatalf("REDIRECT invented a policy-route identity: %v %v", scopes, err)
	}
	plan := planNFTClose(actual)
	if plan.deleteTable == nil || len(plan.deleteChains) != len(program.chains) {
		t.Fatalf("stop left REDIRECT NAT or guard chains behind: %+v", plan)
	}
	wantRules := 0
	for _, chain := range program.chains {
		wantRules += len(chain.rules)
	}
	if len(plan.deleteRules) != wantRules {
		t.Fatalf("stop did not remove all owned rules: got=%d want=%d", len(plan.deleteRules), wantRules)
	}
	// After a normal stop, no firewall ownership remains to turn surviving
	// kernel conntrack entries into a supposed managed routing policy.
	scopes, err = recoverNFTScopes(&nftSnapshot{chains: map[string]*nftChainState{}})
	if err != nil || len(scopes) != 0 {
		t.Fatalf("absent classifier recovered stale REDIRECT ownership: %v %v", scopes, err)
	}
}

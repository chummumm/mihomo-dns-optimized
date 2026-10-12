//go:build linux

package transparentproxy

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/metacubex/nftables"
	"github.com/metacubex/nftables/expr"
	"golang.org/x/sys/unix"
)

func TestNFTRecoveryUsesReachableWorkingMarks(t *testing.T) {
	full := []captureScope{{ModeTProxy, 1, false}, {ModeTUN, 0x162, false}, {ModeTProxy, 1, true}, {ModeTUN, 0x162, true}}
	for _, test := range []struct {
		name   string
		change func(*firewallState)
		want   []captureScope
	}{
		{name: "running", want: full},
		{name: "blocked restart", change: func(s *firewallState) { *s = blockedFirewall(*s) }, want: full},
		{name: "old configured marks", change: func(s *firewallState) { s.TProxyMark, s.TunMark = 0x201, 0x262 }, want: []captureScope{{ModeTProxy, 0x201, false}, {ModeTUN, 0x262, false}, {ModeTProxy, 0x201, true}, {ModeTUN, 0x262, true}}},
		{name: "unused backend", change: func(s *firewallState) { s.TCP = ModeOff; s.IPv6 = false }, want: []captureScope{{ModeTUN, 0x162, false}}},
		{name: "off", change: func(s *firewallState) { s.TCP, s.UDP = ModeOff, ModeOff }},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := nftTestState()
			if test.change != nil {
				test.change(&state)
			}
			program, err := buildNFTRuleset(state)
			if err != nil {
				t.Fatal(err)
			}
			actual := nftSnapshotFromProgram(t, program)
			wall := &nftFirewall{read: func(context.Context) (*nftSnapshot, error) { return actual, nil }}
			scopes, managed, err := wall.RecoverScopes(context.Background())
			if err != nil || !managed || !reflect.DeepEqual(scopes, test.want) {
				t.Fatalf("recovered=%+v managed=%v err=%v; want=%+v", scopes, managed, err, test.want)
			}
		})
	}
}

func TestNFTRecoveryDoesNotInferOrphanOrModifiedMarks(t *testing.T) {
	program, err := buildNFTRuleset(nftTestState())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*nftSnapshot)
		want   []captureScope
	}{
		{name: "orphaned backend", change: func(s *nftSnapshot) { s.chains["prerouting"].rules = nil; s.chains["output"].rules = nil }},
		{name: "changed register", change: func(s *nftSnapshot) {
			expressions := nftMarkVerdict(0x162, nftDrop())
			expressions[0].(*expr.Immediate).Register = 2
			s.chains["tun4"].rules[0].expressions, err = nftRuleFingerprint(&nftables.Rule{Exprs: expressions})
		}, want: []captureScope{{ModeTProxy, 1, false}, {ModeTProxy, 1, true}, {ModeTUN, 0x162, true}}},
		{name: "conflicting marks", change: func(s *nftSnapshot) {
			chain := s.chains["tproxy4"]
			chain.rules[len(chain.rules)-1].expressions, err = nftRuleFingerprint(&nftables.Rule{Exprs: nftMarkVerdict(0x201, nftDrop())})
		}, want: []captureScope{{ModeTUN, 0x162, false}, {ModeTProxy, 1, true}, {ModeTUN, 0x162, true}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual := nftSnapshotFromProgram(t, program)
			test.change(actual)
			if err != nil {
				t.Fatal(err)
			}
			got, err := recoverNFTScopes(actual)
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("guessed a changed/orphan policy: got=%+v want=%+v err=%v", got, test.want, err)
			}
		})
	}
}

func TestNFTRecoveryProbeKeepsUnmanagedAndPermissionFailuresSeparate(t *testing.T) {
	for _, test := range []struct {
		name     string
		snapshot *nftSnapshot
		err      error
		managed  bool
	}{
		{name: "absent", snapshot: &nftSnapshot{}},
		{name: "foreign", snapshot: &nftSnapshot{table: &nftables.Table{Name: nftTableName}, comment: "administrator"}},
		{name: "own empty table", snapshot: &nftSnapshot{table: &nftables.Table{Name: nftTableName}, comment: nftModule}, managed: true},
		{name: "permission denied", err: unix.EPERM},
	} {
		t.Run(test.name, func(t *testing.T) {
			wall := &nftFirewall{read: func(context.Context) (*nftSnapshot, error) { return test.snapshot, test.err }}
			scopes, managed, err := wall.RecoverScopes(context.Background())
			if len(scopes) != 0 || managed != test.managed || !errors.Is(err, test.err) {
				t.Fatalf("probe changed meaning: scopes=%+v managed=%v err=%v", scopes, managed, err)
			}
		})
	}
}

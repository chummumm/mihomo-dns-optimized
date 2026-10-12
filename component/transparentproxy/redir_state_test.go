package transparentproxy

import (
	"errors"
	"testing"
)

func TestRedirReadinessNeedsActualSocketAndDoesNotCreatePolicyRoutes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enable, cfg.TCP, cfg.UDP = true, ModeRedir, ModeOff
	runtime := Runtime{IPv6: true, RedirPort: 7892, RedirTCP: true, RedirTCPIPv6: true}
	policies, state := desiredState(cfg, runtime, testNetwork(), nil, nil)
	if len(policies) != 0 || !state.Redir4 || !state.Redir6 || needsBackendRepair(cfg, runtime, testNetwork()) {
		t.Fatalf("unexpected REDIRECT state: policies=%+v state=%+v", policies, state)
	}
	runtime.RedirTCPIPv6 = false
	_, state = desiredState(cfg, runtime, testNetwork(), nil, nil)
	if !state.Redir4 || state.Redir6 || !needsBackendRepair(cfg, runtime, testNetwork()) {
		t.Fatal("missing IPv6 socket was inferred ready or not repaired")
	}
	network := testNetwork()
	network.IPv6Available = false
	if needsBackendRepair(cfg, runtime, network) {
		t.Fatal("disabled IPv6 stack causes an impossible repair loop")
	}
	runtime.RedirPort = 0
	_, state = desiredState(cfg, runtime, network, nil, nil)
	if state.Redir4 || state.Redir6 || !needsBackendRepair(cfg, runtime, network) {
		t.Fatal("portless backend reported readiness")
	}
}

func TestRedirCloseAndRestartFencesBlockExistingNAT(t *testing.T) {
	m, _, firewall, _ := testManager(t)
	cfg := m.config.Clone()
	cfg.TCP = ModeRedir
	runtime := testRuntime()
	runtime.RedirPort, runtime.RedirTCP, runtime.RedirTCPIPv6 = 7892, true, true
	if err := m.applyConfigurationLocked(cfg, runtime, nil, m.marks); err != nil {
		t.Fatal(err)
	}
	withPublicManager(t, m)
	if !firewall.state.Redir4 || !firewall.state.Redir6 {
		t.Fatal("setup did not enable REDIRECT")
	}
	blocked := errors.New("cannot update guards")
	firewall.applyErr = blocked
	if err := BeforeRedirClose(); !errors.Is(err, blocked) {
		t.Fatalf("close did not propagate capture barrier failure: %v", err)
	}
	if !m.runtime.RedirTCP || !firewall.state.Redir4 {
		t.Fatal("failed barrier discarded live listener readiness")
	}
	firewall.applyErr = nil
	if err := BeforeRedirClose(); err != nil {
		t.Fatal(err)
	}
	if firewall.state.Redir4 || firewall.state.Redir6 || m.runtime.RedirTCP || m.runtime.RedirTCPIPv6 {
		t.Fatal("close barrier left REDIRECT enabled")
	}
	backends := BackendState{RedirPort: 7892, RedirTCP: true, RedirTCPIPv6: true}
	if err := NotifyBackends(backends); err != nil {
		t.Fatal(err)
	}
	if !firewall.state.Redir4 || !firewall.state.Redir6 {
		t.Fatal("ready listener did not restore capture")
	}
	if err := PrepareRestart(); err != nil {
		t.Fatal(err)
	}
	if firewall.state.Redir4 || firewall.state.Redir6 {
		t.Fatal("restart kept REDIRECT sessions enabled")
	}
	if err := NotifyBackends(backends); err != nil {
		t.Fatal(err)
	}
	if firewall.state.Redir4 || firewall.state.Redir6 {
		t.Fatal("backend callback lifted a prepared restart guard")
	}
	if err := AbortRestart(backends); err != nil {
		t.Fatal(err)
	}
	if !firewall.state.Redir4 || !firewall.state.Redir6 {
		t.Fatal("abort did not restore surviving REDIRECT listener")
	}
	if err := Close(); err != nil {
		t.Fatal(err)
	}
	if firewall.state.Enabled {
		t.Fatal("normal stop retained managed capture")
	}
}

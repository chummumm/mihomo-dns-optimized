package listener

import (
	"errors"
	"testing"

	"github.com/metacubex/mihomo/component/transparentproxy"
	C "github.com/metacubex/mihomo/constant"
)

func TestRedirReplacementRequiresGuardAndPublishesActualSocket(t *testing.T) {
	isolateTransparentListeners(t)
	policy := transparentproxy.DefaultConfig()
	policy.Enable, policy.TCP, policy.UDP = true, transparentproxy.ModeRedir, transparentproxy.ModeOff
	transparentConfig = func() transparentproxy.Config { return policy }
	old := &fakeTProxyListener{raw: ":7892", bound: "[::]:7892", ipv4: true, ipv6: true}
	redirListener, lastRedirPort = old, 7892
	var published transparentproxy.BackendState
	notifyBackends = func(state transparentproxy.BackendState) error { published = state; return nil }
	blocked := errors.New("firewall barrier failed")
	beforeRedirClose = func() error { return blocked }
	if err := ReCreateRedir(7893, nil); !errors.Is(err, blocked) {
		t.Fatalf("unsafe replacement failure hidden: %v", err)
	}
	if old.closed != 0 || redirListener != old || published.RedirPort != 7892 || !published.RedirTCPIPv6 {
		t.Fatalf("failed barrier changed live listener: closed=%d state=%+v", old.closed, published)
	}
	guarded, created := false, 0
	beforeRedirClose = func() error { guarded = true; return nil }
	newRedirListener = func(addr string, _ C.Tunnel) (C.Listener, error) {
		if !guarded || old.closed != 1 || addr != ":7893" {
			t.Fatalf("creation preceded guard/close or is loopback-only: %q closed=%d guarded=%v", addr, old.closed, guarded)
		}
		created++
		return &fakeTProxyListener{raw: addr, bound: "[::]:7893", ipv4: true, ipv6: true}, nil
	}
	if err := ReCreateRedir(7893, nil); err != nil {
		t.Fatal(err)
	}
	if published.RedirPort != 7893 || !published.RedirTCP || !published.RedirTCPIPv6 {
		t.Fatalf("new readiness not published: %+v", published)
	}
	if err := ReCreateRedir(7893, nil); err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatal("unchanged reload replaced REDIRECT socket")
	}
}

func TestRedirIPv6RepairAndFailedConstructionRetry(t *testing.T) {
	isolateTransparentListeners(t)
	policy := transparentproxy.DefaultConfig()
	policy.Enable, policy.TCP, policy.UDP = true, transparentproxy.ModeRedir, transparentproxy.ModeOff
	transparentConfig = func() transparentproxy.Config { return policy }
	transparentIPv6Available = func() bool { return true }
	old := &fakeTProxyListener{raw: ":7892", bound: "0.0.0.0:7892", ipv4: true}
	redirListener, lastRedirPort = old, 7892
	barriers, attempts := 0, 0
	beforeRedirClose = func() error { barriers++; return nil }
	newRedirListener = func(addr string, _ C.Tunnel) (C.Listener, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("bind failed")
		}
		return &fakeTProxyListener{raw: addr, bound: "[::]:7892", ipv4: true, ipv6: true}, nil
	}
	if err := RepairTransparentBackends(nil); err == nil {
		t.Fatal("repair construction failure hidden")
	}
	state := TransparentProxyBackends()
	if state.RedirTCP || state.RedirTCPIPv6 || state.RedirPort != 0 || barriers != 1 || old.closed != 1 {
		t.Fatalf("failed replacement advertised readiness: %+v", state)
	}
	if err := RepairTransparentBackends(nil); err != nil {
		t.Fatal(err)
	}
	if !TransparentProxyBackends().RedirTCPIPv6 || attempts != 2 || barriers != 1 {
		t.Fatal("missing listener did not retry independently")
	}
	if err := RepairTransparentBackends(nil); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatal("healthy REDIRECT listener was unnecessarily rebuilt")
	}
	if err := Cleanup(); err != nil {
		t.Fatal(err)
	}
	if TransparentProxyBackends().RedirTCP || barriers != 2 {
		t.Fatal("cleanup did not close behind the REDIRECT guard")
	}
}

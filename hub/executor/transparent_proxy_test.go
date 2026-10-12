package executor

import (
	"errors"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/metacubex/mihomo/component/transparentproxy"
	"github.com/metacubex/mihomo/component/updater"
	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
)

func preserveTransparentProxyIntegration(t *testing.T) {
	t.Helper()
	begin, finish := beginTransparentProxyApply, finishTransparentProxyApply
	closePolicy, restartPolicy := closeTransparentProxy, prepareTransparentRestart
	abort, complete, cleanup := abortTransparentRestart, completeTransparentRestart, cleanupTunListener
	t.Cleanup(func() {
		beginTransparentProxyApply, finishTransparentProxyApply = begin, finish
		closeTransparentProxy, prepareTransparentRestart = closePolicy, restartPolicy
		abortTransparentRestart, completeTransparentRestart, cleanupTunListener = abort, complete, cleanup
	})
}

func TestTransparentApplyFailurePrecedesConfigurationSideEffects(t *testing.T) {
	preserveTransparentProxyIntegration(t)
	preserveUpdateConfiguration(t)
	publishUpdateConfiguration(&config.Config{General: &config.General{}})
	active := CurrentUpdateConfiguration()
	previousLevel := log.Level()
	rejected := errors.New("unable to commit route protection")
	beginTransparentProxyApply = func(stateDir string, cfg transparentproxy.Config, runtime transparentproxy.Runtime) error {
		if stateDir != filepath.Join(C.Path.HomeDir(), "transparent-proxy") {
			t.Fatalf("ownership journal was not isolated to its module directory: %s", stateDir)
		}
		if !cfg.Enable || !runtime.IPv6 {
			t.Fatal("begin did not receive candidate policy and IPv6 intent")
		}
		return rejected
	}
	finishTransparentProxyApply = func(transparentproxy.Runtime) error { t.Fatal("rejected policy reached final publication"); return nil }
	policy := transparentproxy.DefaultConfig()
	policy.Enable = true
	cfg := &config.Config{General: &config.General{TransparentProxy: policy, IPv6: true, LogLevel: log.DEBUG}}
	if err := ApplyConfig(cfg, true); !errors.Is(err, rejected) {
		t.Fatalf("failed route protection was not propagated: %v", err)
	}
	if CurrentUpdateConfiguration() != active || active.Context.Err() != nil || log.Level() != previousLevel {
		t.Fatal("rejected policy invalidated the live configuration or changed global settings")
	}
}

func TestTransparentShutdownBarrierFailurePreservesLiveConfiguration(t *testing.T) {
	for _, operation := range []string{"stop", "restart", "installed update"} {
		t.Run(operation, func(t *testing.T) {
			preserveTransparentProxyIntegration(t)
			preserveUpdateConfiguration(t)
			publishUpdateConfiguration(&config.Config{General: &config.General{}})
			active := CurrentUpdateConfiguration()
			rejected := errors.New("blackhole transaction refused")
			closeCalls, prepareCalls := 0, 0
			closeTransparentProxy = func() error { closeCalls++; return rejected }
			prepareTransparentRestart = func() error { prepareCalls++; return rejected }
			committed, restarted := false, false
			var err error
			switch operation {
			case "stop":
				err = Shutdown()
				if closeCalls != 1 || prepareCalls != 0 {
					t.Fatal("normal stop retained capture as if replacing a process")
				}
			case "restart":
				err = Restart(func() { restarted = true })
				if closeCalls != 0 || prepareCalls != 1 {
					t.Fatal("restart removed capture instead of preparing handoff")
				}
			case "installed update":
				err = ApplyUpdateAndRestart(func() error { committed = true; return nil }, func() { restarted = true })
				if !committed || closeCalls != 0 || prepareCalls != 1 {
					t.Fatal("installed update did not use protected handoff")
				}
				if err == nil || !strings.Contains(err.Error(), "core file installed") || !strings.Contains(err.Error(), "previous core version") {
					t.Fatalf("installed/running version boundary was hidden: %v", err)
				}
			}
			if !errors.Is(err, rejected) || restarted {
				t.Fatalf("failed barrier restarted the process or hid the failure: %v", err)
			}
			if active.Context.Err() != nil || CurrentUpdateConfiguration() != active {
				t.Fatal("failed preparation canceled live update work before TUN could safely stop")
			}
		})
	}
}

func TestControlledRestartProtectsBeforeCancelAndProcessReplacement(t *testing.T) {
	preserveTransparentProxyIntegration(t)
	preserveUpdateConfiguration(t)
	previousUI := updater.CurrentUIUpdater()
	t.Cleanup(func() { updater.SetUIUpdater(previousUI) })
	publishUpdateConfiguration(&config.Config{General: &config.General{}})
	active := CurrentUpdateConfiguration()
	var order []string
	closeTransparentProxy = func() error { t.Fatal("controlled replacement removed capture"); return nil }
	completeTransparentRestart = func() { order = append(order, "complete handoff") }
	prepareTransparentRestart = func() error {
		if active.Context.Err() != nil {
			t.Fatal("updates were canceled before restart protection succeeded")
		}
		order = append(order, "retain capture and blackholes")
		return nil
	}
	if err := Restart(func() {
		if active.Context.Err() == nil {
			t.Fatal("reexec began before background update work was canceled")
		}
		order = append(order, "replace process")
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"retain capture and blackholes", "complete handoff", "replace process"}) {
		t.Fatalf("unsafe restart lifecycle order: %v", order)
	}
}

func TestRestartCleanupFailureAbortsHandoffBeforeCancelingOtherWork(t *testing.T) {
	preserveTransparentProxyIntegration(t)
	preserveUpdateConfiguration(t)
	previousUI := updater.CurrentUIUpdater()
	t.Cleanup(func() { updater.SetUIUpdater(previousUI) })
	publishUpdateConfiguration(&config.Config{General: &config.General{}})
	active := CurrentUpdateConfiguration()
	cleanupErr := errors.New("old TUN close failed")
	failed := true
	var order []string
	prepareTransparentRestart = func() error { order = append(order, "prepare"); return nil }
	closeTransparentProxy = func() error { t.Fatal("restart removed capture"); return nil }
	cleanupTunListener = func() error {
		order = append(order, "close TUN")
		if failed {
			return cleanupErr
		}
		return nil
	}
	abortTransparentRestart = func(state transparentproxy.BackendState) error {
		if active.Context.Err() != nil {
			t.Fatal("cleanup failure canceled the live configuration before recovery")
		}
		order = append(order, "restore actual backends")
		return nil
	}
	completeTransparentRestart = func() { order = append(order, "complete") }
	restart := func() { order = append(order, "exec") }
	if err := Restart(restart); !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup failure was hidden: %v", err)
	}
	if active.Context.Err() != nil || !reflect.DeepEqual(order, []string{"prepare", "close TUN", "restore actual backends"}) {
		t.Fatalf("failed close abandoned the running configuration: %v", order)
	}
	failed, order = false, nil
	if err := Restart(restart); err != nil {
		t.Fatal(err)
	}
	if active.Context.Err() == nil || !reflect.DeepEqual(order, []string{"prepare", "close TUN", "complete", "exec"}) {
		t.Fatalf("retry bypassed the protection or final shutdown boundary: %v", order)
	}
}

func TestTransparentRuntimeUsesOnlyActiveFakeIPPools(t *testing.T) {
	v4, v6 := netip.MustParsePrefix("198.18.0.1/16"), netip.MustParsePrefix("fc00::1/16")
	cfg := &config.Config{General: &config.General{IPv6: true}, DNS: &config.DNS{Enable: true, EnhancedMode: C.DNSFakeIP, FakeIPRange: v4, FakeIPRange6: v6}}
	if got := transparentProxyRuntime(cfg).FakeIP; !reflect.DeepEqual(got, []netip.Prefix{v4.Masked(), v6.Masked()}) {
		t.Fatalf("active Fake-IP pools were not passed before reserved-prefix bypass: %v", got)
	}
	cfg.General.IPv6 = false
	if got := transparentProxyRuntime(cfg).FakeIP; !reflect.DeepEqual(got, []netip.Prefix{v4.Masked()}) {
		t.Fatalf("disabled IPv6 retained a capture-only Fake-IP pool: %v", got)
	}
	cfg.DNS.EnhancedMode = C.DNSMapping
	if len(transparentProxyRuntime(cfg).FakeIP) != 0 {
		t.Fatal("redir-host mode published inactive Fake-IP ranges")
	}
	cfg.DNS.EnhancedMode, cfg.DNS.Enable = C.DNSFakeIP, false
	if len(transparentProxyRuntime(cfg).FakeIP) != 0 {
		t.Fatal("disabled DNS published inactive Fake-IP ranges")
	}
}

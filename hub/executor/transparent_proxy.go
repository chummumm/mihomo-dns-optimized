package executor

import (
	"errors"
	"net/netip"
	"sync"

	"github.com/metacubex/mihomo/component/transparentproxy"
	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/listener"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/tunnel"
)

var (
	beginTransparentProxyApply  = transparentproxy.BeginApply
	finishTransparentProxyApply = transparentproxy.Configure
	closeTransparentProxy       = transparentproxy.Close
	prepareTransparentRestart   = transparentproxy.PrepareRestart
	abortTransparentRestart     = transparentproxy.AbortRestart
	completeTransparentRestart  = transparentproxy.CompleteRestart
	cleanupTunListener          = listener.Cleanup
	transparentProxyRepairsOnce sync.Once
)

func transparentProxyRuntime(cfg *config.Config) transparentproxy.Runtime {
	backends := listener.TransparentProxyBackends()
	runtime := transparentproxy.Runtime{
		IPv6:             cfg.General.IPv6,
		TunIPv6Requested: len(cfg.General.Tun.Inet6Address) != 0,
		TunName:          backends.TunName, TunIndex: backends.TunIndex, TunReady: backends.TunReady,
		TunIPv6Ready: backends.TunIPv6Ready,
		TProxyPort:   backends.TProxyPort, TProxyTCP: backends.TProxyTCP, TProxyUDP: backends.TProxyUDP,
		TProxyTCPIPv6: backends.TProxyTCPIPv6, TProxyUDPIPv6: backends.TProxyUDPIPv6,
	}
	if cfg.DNS != nil && cfg.DNS.Enable && cfg.DNS.EnhancedMode == C.DNSFakeIP {
		for _, prefix := range []netip.Prefix{cfg.DNS.FakeIPRange, cfg.DNS.FakeIPRange6} {
			if prefix.IsValid() && (prefix.Addr().Is4() || cfg.General.IPv6) {
				runtime.FakeIP = append(runtime.FakeIP, prefix.Masked())
			}
		}
	}
	return runtime
}

// ApplyConfigPatch serializes controller changes with YAML replacement and
// backend recovery. The patch itself owns its validation and HTTP contract.
func ApplyConfigPatch(patch func() error) error {
	mux.Lock()
	defer mux.Unlock()
	if ShutdownRequested() {
		return ErrShuttingDown
	}
	return patch()
}

func startTransparentProxyRepairs() {
	transparentProxyRepairsOnce.Do(func() {
		go func() {
			for range transparentproxy.BackendRepairRequests() {
				// Configuration replacement, shutdown and local recovery share
				// the same ordering. The manager only sends nonblocking requests
				// and never waits for this consumer while holding its own lock.
				err := ApplyConfigPatch(func() error {
					if !transparentproxy.IsEnabled() {
						return nil
					}
					return listener.RepairTransparentBackends(tunnel.Tunnel)
				})
				if err != nil && !errors.Is(err, ErrShuttingDown) {
					log.Warnln("[Transparent Proxy] backend recovery: %s", err)
				}
			}
		}()
	})
}

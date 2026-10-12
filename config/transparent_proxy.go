package config

import (
	"fmt"
	"runtime"

	"github.com/metacubex/mihomo/component/transparentproxy"
)

// Validate backend dependencies before candidate parsing temporarily changes
// network globals or initializes providers. Validation never creates listeners
// or changes the caller's YAML values.
func validateTransparentProxy(raw *RawConfig) error {
	cfg := raw.TransparentProxy
	if err := cfg.Validate(); err != nil {
		return err
	}
	if !cfg.Enable {
		return nil
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("transparent-proxy is only supported on Linux")
	}
	if cfg.Uses(transparentproxy.ModeTUN) && !raw.Tun.Enable {
		return fmt.Errorf("transparent-proxy selects tun, which requires tun.enable: true")
	}
	if cfg.Uses(transparentproxy.ModeTProxy) && (raw.TProxyPort < 1 || raw.TProxyPort > 65535) {
		return fmt.Errorf("transparent-proxy selects tproxy, which requires tproxy-port: 1..65535")
	}
	if cfg.Uses(transparentproxy.ModeRedir) && (raw.RedirPort < 1 || raw.RedirPort > 65535) {
		return fmt.Errorf("transparent-proxy selects redir, which requires redir-port: 1..65535")
	}
	if raw.RoutingMark < 0 || uint64(raw.RoutingMark) > uint64(^uint32(0)) {
		return fmt.Errorf("routing-mark must be a 32-bit unsigned integer when transparent-proxy is enabled")
	}
	mark := uint32(raw.RoutingMark)
	if mark == transparentproxy.RedirMark || mark != 0 && ((cfg.Uses(transparentproxy.ModeTProxy) && mark == cfg.Routing.TProxy.Mark) ||
		(cfg.Uses(transparentproxy.ModeTUN) && mark == cfg.Routing.TUN.Mark)) {
		return fmt.Errorf("routing-mark must differ from the transparent-proxy capture marks")
	}
	return nil
}

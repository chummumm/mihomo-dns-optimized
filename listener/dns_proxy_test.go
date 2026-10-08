package listener_test

import (
	"strings"
	"testing"

	"github.com/metacubex/mihomo/listener"
	"github.com/metacubex/mihomo/listener/inbound"
)

func TestDNSProxyListenerDefaultsAndDisabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "default-enabled"
		mapping := map[string]any{"name": "smartdns", "type": "dns-proxy", "port": 0}
		if !enabled {
			name = "explicitly-disabled"
			mapping["enable"] = false
		}
		t.Run(name, func(t *testing.T) {
			l, err := listener.ParseListener(mapping)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			if l.RawAddress() != "127.0.0.1:0" {
				t.Fatalf("DNS proxy did not default to loopback: %s", l.RawAddress())
			}
			config := l.Config().(*inbound.DNSProxyOption)
			if config.Enable != enabled {
				t.Fatalf("enable=%v, want %v", config.Enable, enabled)
			}
			// A disabled listener must neither require a DNS exchanger nor open
			// a socket. The enabled variant refuses an incompatible tunnel.
			err = l.Listen(nil)
			if !enabled && err != nil {
				t.Fatalf("disabled listener attempted to start: %v", err)
			}
			if enabled && err == nil {
				t.Fatal("enabled listener accepted a tunnel without DNS exchange")
			}
			if l.Address() != "" {
				t.Fatalf("unexpected active socket: %s", l.Address())
			}
		})
	}
}

func TestDNSProxyListenerRejectsUnsupportedRouteOverrides(t *testing.T) {
	for _, key := range []string{"rule", "proxy"} {
		t.Run(key, func(t *testing.T) {
			_, err := listener.ParseListener(map[string]any{
				"name": "smartdns", "type": "dns-proxy", "port": 7898,
				key: "another-route",
			})
			if err == nil || !strings.Contains(err.Error(), "global rules") {
				t.Fatalf("unsupported %s override was not clearly rejected: %v", key, err)
			}
		})
	}
}

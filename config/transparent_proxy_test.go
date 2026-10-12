package config

import (
	"net/netip"
	"runtime"
	"strings"
	"testing"

	"github.com/metacubex/mihomo/component/transparentproxy"
)

func TestTransparentProxyNormalizesOnlyEffectiveCaptureSwitches(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("managed capture is Linux-only")
	}
	raw, err := UnmarshalRawConfig([]byte(`
tproxy-port: 7895
tun:
  enable: true
  device: utun
  auto-route: true
  auto-redirect: true
iptables:
  enable: true
transparent-proxy:
  enable: true
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, managed := range []bool{true, false, true} {
		raw.TransparentProxy.Enable = managed
		general, err := parseGeneral(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := parseTun(raw.Tun, &DNS{}, general); err != nil {
			t.Fatal(err)
		}
		iptables, err := parseIPTables(raw)
		if err != nil {
			t.Fatal(err)
		}
		if general.Tun.AutoRoute == managed || general.Tun.AutoRedirect == managed || iptables.Enable == managed {
			t.Fatalf("incorrect effective switches, managed=%v: TUN=%+v iptables=%+v", managed, general.Tun, iptables)
		}
		if !raw.Tun.AutoRoute || !raw.Tun.AutoRedirect || !raw.IPTables.Enable || !general.Tun.AutoDetectInterface {
			t.Fatal("normalization changed raw settings or auto-detect-interface")
		}
		if general.TransparentProxy.TCP != transparentproxy.ModeTProxy || general.TransparentProxy.UDP != transparentproxy.ModeTUN {
			t.Fatal("omitted backends did not inherit the managed defaults")
		}
		general.TransparentProxy.Local.Interfaces[0] = "changed-snapshot"
		if raw.TransparentProxy.Local.Interfaces[0] != "auto" {
			t.Fatal("parsed policy shares writable slices with its source")
		}
	}
}

func TestTransparentProxyDependenciesFailBeforeParsingResources(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("managed capture is Linux-only")
	}
	for _, test := range []struct {
		name, want string
		change     func(*RawConfig)
	}{
		{"missing TUN", "tun.enable", func(raw *RawConfig) { raw.Tun.Enable = false }},
		{"missing TProxy", "tproxy-port", func(raw *RawConfig) { raw.TProxyPort = 0 }},
		{"overflow port", "tproxy-port", func(raw *RawConfig) { raw.TProxyPort = 65536 }},
		{"TUN capture mark", "capture marks", func(raw *RawConfig) { raw.RoutingMark = 0x162 }},
		{"TProxy capture mark", "capture marks", func(raw *RawConfig) { raw.RoutingMark = 1 }},
		{"negative global mark", "32-bit", func(raw *RawConfig) { raw.RoutingMark = -1 }},
		{"reserved REDIRECT mark", "capture marks", func(raw *RawConfig) { raw.RoutingMark = int(transparentproxy.RedirMark) }},
		{"missing REDIRECT", "redir-port", func(raw *RawConfig) { raw.TransparentProxy.TCP = transparentproxy.ModeRedir; raw.RedirPort = 0 }},
		{"overflow REDIRECT", "redir-port", func(raw *RawConfig) { raw.TransparentProxy.TCP = transparentproxy.ModeRedir; raw.RedirPort = 65536 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := DefaultRawConfig()
			raw.TransparentProxy.Enable = true
			raw.Tun.Enable = true
			raw.TProxyPort = 7895
			test.change(raw)
			if _, err := parseGeneral(raw); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("dependency was not rejected: %v", err)
			}
			raw.TransparentProxy.Enable = false
			if _, err := parseGeneral(raw); err != nil {
				t.Fatalf("disabled feature changed the legacy parser: %v", err)
			}
		})
	}
	for _, mode := range []transparentproxy.Mode{transparentproxy.ModeTUN, transparentproxy.ModeTProxy, transparentproxy.ModeOff} {
		raw := DefaultRawConfig()
		raw.TransparentProxy.Enable = true
		raw.TransparentProxy.TCP, raw.TransparentProxy.UDP = mode, mode
		raw.Tun.Enable = mode == transparentproxy.ModeTUN
		if mode == transparentproxy.ModeTProxy {
			raw.TProxyPort = 7895
		}
		if _, err := parseGeneral(raw); err != nil {
			t.Fatalf("inactive backend dependency was still required for %s: %v", mode, err)
		}
	}
}

func TestTransparentProxyRedirOnlyRequiresItsOwnTCPListener(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("managed capture is Linux-only")
	}
	raw := DefaultRawConfig()
	raw.TransparentProxy.Enable = true
	raw.TransparentProxy.TCP, raw.TransparentProxy.UDP = transparentproxy.ModeRedir, transparentproxy.ModeOff
	raw.RedirPort = 7892
	if _, err := parseGeneral(raw); err != nil {
		t.Fatalf("TCP REDIRECT required an unrelated backend: %v", err)
	}
}

func TestTransparentProxyKeepsIPv6IntentUntilExplicitlyDisabled(t *testing.T) {
	raw := DefaultRawConfig()
	raw.TransparentProxy.Enable = true
	raw.DNS.FakeIPRange6 = "fdfe:dcba:9876::1/64"
	wanted := netip.MustParsePrefix("fdfe:dcba:9876::1/126")
	raw.Tun.Inet6Address = []netip.Prefix{wanted}
	parseIPV6(raw)
	if raw.DNS.FakeIPRange6 == "" || len(raw.Tun.Inet6Address) != 1 || raw.Tun.Inet6Address[0] != wanted {
		t.Fatal("boot-time availability discarded the configured IPv6 request")
	}
	raw.IPv6 = false
	parseIPV6(raw)
	if raw.DNS.FakeIPRange6 != "" || len(raw.Tun.Inet6Address) != 0 {
		t.Fatal("managed capture ignored an explicit ipv6: false")
	}
}

func TestTransparentProxyUnsupportedPlatformLeavesDefaultDisabled(t *testing.T) {
	raw := DefaultRawConfig()
	if raw.TransparentProxy.Enable {
		t.Fatal("managed capture was enabled by omission")
	}
	if _, err := parseGeneral(raw); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" {
		raw.TransparentProxy.Enable = true
		if _, err := parseGeneral(raw); err == nil || !strings.Contains(err.Error(), "Linux") {
			t.Fatalf("unsupported platform was not rejected: %v", err)
		}
	}
}

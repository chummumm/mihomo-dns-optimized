package transparentproxy

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestConfigPartialDefaultsAndIndependentBackends(t *testing.T) {
	var cfg Config
	err := yaml.Unmarshal([]byte("enable: true\ntcp: tun\nudp: off\nrouting:\n  tun:\n    table: 123\nlocal:\n  interfaces: [eth0]\n"), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TCP != ModeTUN || cfg.UDP != ModeOff || cfg.Routing.TUN.Table != 123 || cfg.Routing.TUN.Mark != 0x162 || cfg.Routing.TUN.Priority != 1010 {
		t.Fatalf("partial routing defaults lost: %+v", cfg)
	}
	if !cfg.Local.Enable || len(cfg.Local.BypassUDPPorts) != 1 || cfg.Local.BypassUDPPorts[0] != 123 || !cfg.Bypass.ReservedAddresses {
		t.Fatalf("nested defaults lost: %+v", cfg)
	}
	clone := cfg.Clone()
	clone.Local.Interfaces[0] = "changed"
	clone.Local.BypassUDPPorts[0] = 53
	if cfg.Local.Interfaces[0] != "eth0" || cfg.Local.BypassUDPPorts[0] != 123 {
		t.Fatal("configuration clone aliases mutable slices")
	}
}

func TestConfigRejectsAmbiguousNumbersAndUnknownOptions(t *testing.T) {
	cases := []string{
		"reconcile-interval: 1m",
		"tcp: none",
		"tcp: true",
		"routing: {tun: {mark: 1.5}}",
		"routing: {tun: {mark: -1}}",
		"routing: {tun: {mark: 4294967296}}",
		"routing: {tun: {mark: '354'}}",
		"routing: {tun: {mark: 0}}",
		"routing: {tun: {table: 254}}",
		"routing: {tun: {priority: 32765}}",
		"routing: {tun: {priority: 1001}}",
		"routing: {tun: {mark: 0x1}}",
		"routing: {tun: {table: 100}}",
		"local: {bypass-udp-ports: [53.2]}",
		"local: {bypass-udp-ports: [65536]}",
		"local: {bypass-udp-ports: [0]}",
		"bypass: {extra-addresses: ['0.0.0.0/0']}",
		"bypass: {interface-prefixes: ['../eth0']}",
	}
	for _, value := range cases {
		t.Run(strings.ReplaceAll(value, "/", "_"), func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte("enable: true\n"+value+"\n"), &cfg); err == nil {
				t.Fatalf("accepted invalid config: %s", value)
			}
		})
	}
}

func TestOutboundMarkConflictUsesExactValue(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enable = true
	for _, mark := range []uint32{0, DefaultOutboundMark, 0x163, 0xffff0001} {
		if err := validateOutboundMark(cfg, mark); err != nil {
			t.Fatalf("unrelated complete mark %#x rejected: %v", mark, err)
		}
	}
	for _, mark := range []uint32{1, 0x162} {
		if err := validateOutboundMark(cfg, mark); err == nil {
			t.Fatalf("capture mark %#x accepted for outbound", mark)
		}
	}
}

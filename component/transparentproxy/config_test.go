package transparentproxy

import (
	"encoding/json"
	"reflect"
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
	if !cfg.Local.Enable || len(cfg.Local.BypassPorts.UDP) != 1 || cfg.Local.BypassPorts.UDP[0] != 123 || !cfg.Bypass.ReservedAddresses {
		t.Fatalf("nested defaults lost: %+v", cfg)
	}
	clone := cfg.Clone()
	clone.Local.Interfaces[0] = "changed"
	clone.Local.BypassPorts.UDP[0] = 53
	if cfg.Local.Interfaces[0] != "eth0" || cfg.Local.BypassPorts.UDP[0] != 123 {
		t.Fatal("configuration clone aliases mutable slices")
	}
}

func TestConfigRejectsAmbiguousNumbersAndUnknownOptions(t *testing.T) {
	cases := []string{
		"reconcile-interval: 1m",
		"tcp: none",
		"tcp: true",
		"udp: redir",
		"tcp: mixed",
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
		"local: {bypass-ports: {tcp: [0]}}",
		"local: {bypass-ports: {udp: [65536]}}",
		"local: {bypass-ports: {tcp: ['443']}}",
		"local: {bypass-ports: {tcp: [443.5]}}",
		"local: {bypass-ports: {tcp: [443], tcp: [80]}}",
		"local: {bypass-ports: {tcp: [443]}, bypass-udp-ports: [123]}",
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
	for _, mark := range []uint32{1, 0x162, RedirMark} {
		if err := validateOutboundMark(cfg, mark); err == nil {
			t.Fatalf("capture mark %#x accepted for outbound", mark)
		}
	}
}

func TestLocalBypassPortsDefaultsCompatibilityAndSerialization(t *testing.T) {
	for _, test := range []struct {
		name, yaml string
		tcp, udp   []uint16
		users      []string
	}{
		{name: "defaults", tcp: []uint16{}, udp: []uint16{123}, users: []string{"mihomo"}},
		{name: "independent defaults", yaml: "local: {bypass-ports: {tcp: [80, 443]}}", tcp: []uint16{80, 443}, udp: []uint16{123}, users: []string{"mihomo"}},
		{name: "explicit disable", yaml: "local: {bypass-users: [], bypass-ports: {tcp: [], udp: []}}", tcp: []uint16{}, udp: []uint16{}, users: []string{}},
		{name: "empty values", yaml: "local:\n  bypass-ports:\n    tcp:\n    udp:\n", tcp: []uint16{}, udp: []uint16{}, users: []string{"mihomo"}},
		{name: "legacy UDP alias", yaml: "local: {bypass-udp-ports: [53, 123]}", tcp: []uint16{}, udp: []uint16{53, 123}, users: []string{"mihomo"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte("enable: true\n"+test.yaml), &cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Local.BypassPorts.TCP, test.tcp) || !reflect.DeepEqual(cfg.Local.BypassPorts.UDP, test.udp) || !reflect.DeepEqual(cfg.Local.BypassUsers, test.users) {
				t.Fatalf("incorrect local defaults or explicit override: %+v", cfg.Local)
			}
			for _, marshal := range []func(any) ([]byte, error){json.Marshal, yaml.Marshal} {
				serialized, err := marshal(cfg)
				if err != nil || !strings.Contains(string(serialized), "bypass-ports") || strings.Contains(string(serialized), "bypass-udp-ports") {
					t.Fatalf("deprecated parser alias escaped into config API: %s %v", serialized, err)
				}
			}
		})
	}
}

func TestTCPRedirHasNoRoutingPolicyAndReservesInternalMark(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enable, cfg.TCP, cfg.UDP = true, ModeRedir, ModeOff
	cfg.Routing.TProxy, cfg.Routing.TUN = RouteConfig{}, RouteConfig{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("REDIRECT unnecessarily requires a policy-routing table: %v", err)
	}
	for _, mode := range []Mode{ModeTUN, ModeTProxy} {
		cfg := DefaultConfig()
		cfg.Enable = true
		if mode == ModeTUN {
			cfg.Routing.TUN.Mark = RedirMark
		} else {
			cfg.Routing.TProxy.Mark = RedirMark
		}
		if err := cfg.Validate(); err == nil {
			t.Fatal("reserved REDIRECT mark accepted for policy routing")
		}
	}
}

func TestMissingBypassUserHasActionableError(t *testing.T) {
	_, err := resolveBypassUsers([]string{"mihomo-transparent-proxy-missing-test-user-447823"})
	if err == nil || !strings.Contains(err.Error(), "bypass-users: []") {
		t.Fatalf("missing service account did not explain how to configure an alternative: %v", err)
	}
}

package transparentproxy

import (
	"fmt"
	"net/netip"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/exp/slices"
)

type Mode string

const (
	ModeOff    Mode = "off"
	ModeTProxy Mode = "tproxy"
	ModeTUN    Mode = "tun"
	// DefaultOutboundMark is separate from both capture marks. An explicit
	// routing-mark always retains its entire value; bits are never ORed into it.
	DefaultOutboundMark uint32 = 0x4d484d4f
)

type Config struct {
	Enable  bool          `yaml:"enable" json:"enable"`
	TCP     Mode          `yaml:"tcp" json:"tcp"`
	UDP     Mode          `yaml:"udp" json:"udp"`
	Routing RoutingConfig `yaml:"routing" json:"routing"`
	Local   LocalConfig   `yaml:"local" json:"local"`
	Gateway GatewayConfig `yaml:"gateway" json:"gateway"`
	Bypass  BypassConfig  `yaml:"bypass" json:"bypass"`
}

type RoutingConfig struct {
	TProxy RouteConfig `yaml:"tproxy" json:"tproxy"`
	TUN    RouteConfig `yaml:"tun" json:"tun"`
}

type RouteConfig struct {
	Mark     uint32 `yaml:"mark" json:"mark"`
	Table    uint32 `yaml:"table" json:"table"`
	Priority uint32 `yaml:"priority" json:"priority"`
}

type LocalConfig struct {
	Enable         bool     `yaml:"enable" json:"enable"`
	Interfaces     []string `yaml:"interfaces" json:"interfaces"`
	BypassUsers    []string `yaml:"bypass-users" json:"bypass-users"`
	BypassUDPPorts []uint16 `yaml:"bypass-udp-ports" json:"bypass-udp-ports"`
}

type GatewayConfig struct {
	Enable     bool     `yaml:"enable" json:"enable"`
	Interfaces []string `yaml:"interfaces" json:"interfaces"`
}

type BypassConfig struct {
	ReservedAddresses bool     `yaml:"reserved-addresses" json:"reserved-addresses"`
	InterfacePrefixes []string `yaml:"interface-prefixes" json:"interface-prefixes"`
	ExtraAddresses    []string `yaml:"extra-addresses" json:"extra-addresses"`
}

func DefaultConfig() Config {
	return Config{
		TCP: ModeTProxy, UDP: ModeTUN,
		Routing: RoutingConfig{
			TProxy: RouteConfig{Mark: 1, Table: 100, Priority: 1000},
			TUN:    RouteConfig{Mark: 0x162, Table: 101, Priority: 1010},
		},
		Local:   LocalConfig{Enable: true, Interfaces: []string{"auto"}, BypassUDPPorts: []uint16{123}},
		Gateway: GatewayConfig{Interfaces: []string{"*"}},
		Bypass:  BypassConfig{ReservedAddresses: true, InterfacePrefixes: []string{"auto"}},
	}
}

func (c Config) Uses(mode Mode) bool { return c.Enable && (c.TCP == mode || c.UDP == mode) }

func (c Config) Clone() Config {
	c.Local.Interfaces = slices.Clone(c.Local.Interfaces)
	c.Local.BypassUsers = slices.Clone(c.Local.BypassUsers)
	c.Local.BypassUDPPorts = slices.Clone(c.Local.BypassUDPPorts)
	c.Gateway.Interfaces = slices.Clone(c.Gateway.Interfaces)
	c.Bypass.InterfacePrefixes = slices.Clone(c.Bypass.InterfacePrefixes)
	c.Bypass.ExtraAddresses = slices.Clone(c.Bypass.ExtraAddresses)
	return c
}

func (c Config) Validate() error {
	if !c.Enable {
		return nil
	}
	for _, entry := range []struct {
		name string
		mode Mode
	}{{"tcp", c.TCP}, {"udp", c.UDP}} {
		if entry.mode != ModeOff && entry.mode != ModeTProxy && entry.mode != ModeTUN {
			return fmt.Errorf("transparent-proxy.%s must be tproxy, tun or off", entry.name)
		}
	}
	for _, entry := range []struct {
		mode  Mode
		route RouteConfig
	}{{ModeTProxy, c.Routing.TProxy}, {ModeTUN, c.Routing.TUN}} {
		if !c.Uses(entry.mode) {
			continue
		}
		p := entry.route
		if p.Mark == 0 || p.Mark == DefaultOutboundMark {
			return fmt.Errorf("transparent-proxy.routing.%s.mark must be nonzero and differ from internal outbound mark 0x%x", entry.mode, DefaultOutboundMark)
		}
		if p.Table == 0 || p.Table == 253 || p.Table == 254 || p.Table == 255 {
			return fmt.Errorf("transparent-proxy.routing.%s.table cannot use a reserved routing table", entry.mode)
		}
		// Each lookup owns the following priority for a terminal blackhole rule.
		// Both must precede the standard main-table lookup at 32766.
		if p.Priority == 0 || p.Priority > 32764 {
			return fmt.Errorf("transparent-proxy.routing.%s.priority must be 1..32764 (the following priority is reserved for its blackhole guard)", entry.mode)
		}
	}
	if c.Uses(ModeTProxy) && c.Uses(ModeTUN) {
		a, b := c.Routing.TProxy, c.Routing.TUN
		if a.Mark == b.Mark || a.Table == b.Table {
			return fmt.Errorf("transparent-proxy TProxy and TUN must use different marks and routing tables")
		}
		if a.Priority <= b.Priority+1 && b.Priority <= a.Priority+1 {
			return fmt.Errorf("transparent-proxy routing priority pairs overlap")
		}
	}
	for _, entry := range []struct {
		name   string
		values []string
	}{{"local.interfaces", c.Local.Interfaces}, {"gateway.interfaces", c.Gateway.Interfaces}, {"bypass.interface-prefixes", c.Bypass.InterfacePrefixes}} {
		for _, name := range entry.values {
			if name == "auto" || name == "*" {
				continue
			}
			if name == "" || len(name) > 15 || name == "." || name == ".." || strings.ContainsAny(name, "/\x00 \t\r\n") {
				return fmt.Errorf("transparent-proxy.%s contains an invalid interface name", entry.name)
			}
		}
	}
	for _, prefix := range c.Bypass.ExtraAddresses {
		p, err := netip.ParsePrefix(prefix)
		if err != nil || p.Bits() == 0 || p.Addr().Is4In6() {
			return fmt.Errorf("transparent-proxy.bypass.extra-addresses requires non-default IPv4 or IPv6 prefixes: %q", prefix)
		}
	}
	for _, port := range c.Local.BypassUDPPorts {
		if port == 0 {
			return fmt.Errorf("transparent-proxy.local.bypass-udp-ports must be 1..65535")
		}
	}
	for _, name := range c.Local.BypassUsers {
		if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "\x00\r\n") {
			return fmt.Errorf("transparent-proxy.local.bypass-users contains an invalid user")
		}
	}
	return nil
}

func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	if err := knownFields(node, "enable", "tcp", "udp", "routing", "local", "gateway", "bypass"); err != nil {
		return err
	}
	type plain Config
	value := plain(DefaultConfig())
	if err := node.Decode(&value); err != nil {
		return err
	}
	*c = Config(value)
	return c.Validate()
}

func (m *Mode) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return fmt.Errorf("transparent-proxy backend must be tproxy, tun or quoted off")
	}
	*m = Mode(node.Value)
	if *m != ModeOff && *m != ModeTProxy && *m != ModeTUN {
		return fmt.Errorf("invalid transparent-proxy backend %q", node.Value)
	}
	return nil
}

func (r *RoutingConfig) UnmarshalYAML(node *yaml.Node) error {
	if err := knownFields(node, "tproxy", "tun"); err != nil {
		return err
	}
	type plain RoutingConfig
	value := plain(*r)
	if err := node.Decode(&value); err != nil {
		return err
	}
	*r = RoutingConfig(value)
	return nil
}

func (r *RouteConfig) UnmarshalYAML(node *yaml.Node) error {
	if err := knownFields(node, "mark", "table", "priority"); err != nil {
		return err
	}
	for i := 1; i < len(node.Content); i += 2 {
		if node.Content[i].Tag != "!!int" {
			return fmt.Errorf("transparent-proxy route mark, table and priority require unsigned integers")
		}
	}
	type plain RouteConfig
	value := plain(*r)
	if err := node.Decode(&value); err != nil {
		return err
	}
	*r = RouteConfig(value)
	return nil
}

func (l *LocalConfig) UnmarshalYAML(node *yaml.Node) error {
	if err := knownFields(node, "enable", "interfaces", "bypass-users", "bypass-udp-ports"); err != nil {
		return err
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value != "bypass-udp-ports" {
			continue
		}
		for _, port := range node.Content[i+1].Content {
			if port.Tag != "!!int" {
				return fmt.Errorf("transparent-proxy bypass UDP ports require integers")
			}
		}
	}
	type plain LocalConfig
	value := plain(*l)
	if err := node.Decode(&value); err != nil {
		return err
	}
	*l = LocalConfig(value)
	return nil
}

func (g *GatewayConfig) UnmarshalYAML(node *yaml.Node) error {
	if err := knownFields(node, "enable", "interfaces"); err != nil {
		return err
	}
	type plain GatewayConfig
	value := plain(*g)
	if err := node.Decode(&value); err != nil {
		return err
	}
	*g = GatewayConfig(value)
	return nil
}

func (b *BypassConfig) UnmarshalYAML(node *yaml.Node) error {
	if err := knownFields(node, "reserved-addresses", "interface-prefixes", "extra-addresses"); err != nil {
		return err
	}
	type plain BypassConfig
	value := plain(*b)
	if err := node.Decode(&value); err != nil {
		return err
	}
	*b = BypassConfig(value)
	return nil
}

func knownFields(node *yaml.Node, names ...string) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("transparent-proxy configuration must be a mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		if !slices.Contains(names, node.Content[i].Value) {
			return fmt.Errorf("unknown transparent-proxy configuration field %q", node.Content[i].Value)
		}
	}
	return nil
}

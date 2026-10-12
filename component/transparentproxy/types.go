// Package transparentproxy owns Linux transparent capture, policy routes and
// their lifecycle. Callers publish configuration and listener readiness; the
// package keeps system events, ownership and recovery behind that boundary.
package transparentproxy

import (
	"context"
	"io"
	"net/netip"
)

// Runtime describes the applied resolver and the actual default listeners.
// TunIndex identifies the live device, not merely a reusable interface name.
type Runtime struct {
	IPv6         bool
	TunName      string
	TunIndex     int
	TunReady     bool
	TunIPv6Ready bool
	// Requested is separate from availability: an explicitly empty TUN IPv6
	// address list must stay blocked without starting an endless repair loop.
	TunIPv6Requested bool
	TProxyPort       uint16
	TProxyTCP        bool
	TProxyUDP        bool
	TProxyTCPIPv6    bool
	TProxyUDPIPv6    bool
	FakeIP           []netip.Prefix
}

// BackendState can be published while a listener lock is held. No callback from
// this package acquires a listener lock or reads mutable listener internals.
type BackendState struct {
	TunName       string
	TunIndex      int
	TunReady      bool
	TunIPv6Ready  bool
	TProxyPort    uint16
	TProxyTCP     bool
	TProxyUDP     bool
	TProxyTCPIPv6 bool
	TProxyUDPIPv6 bool
}

type routePolicy struct {
	Backend  Mode
	Mark     uint32
	Table    uint32
	Priority uint32
	IPv6     bool
	IfIndex  int
	Ready    bool
}

// captureScope is read from an existing packet-classification rule. It is not
// a separate recovery record; these are the actual marks used by the firewall.
type captureScope struct {
	Backend Mode
	Mark    uint32
	IPv6    bool
}

type networkInterface struct {
	Index    int
	Name     string
	Up       bool
	Loopback bool
	// Addresses contains usable addresses with their real prefix lengths.
	// Deprecated but still valid addresses remain; tentative/failed ones do not.
	Addresses []netip.Prefix
}

type networkState struct {
	Interfaces  []networkInterface
	DefaultIPv4 string
	DefaultIPv6 string
	// IPv6Available describes the namespace's IPv6 stack, not Internet reachability.
	IPv6Available bool
}

type systemEvent struct{ Err error }

type routeSystem interface {
	Watch(context.Context) (<-chan systemEvent, io.Closer, error)
	Snapshot(context.Context) (networkState, error)
	RecoverRoutes(context.Context, []routePolicy, []captureScope) error
	EnsureRoutes(context.Context, []routePolicy) error
	PruneRoutes(context.Context, []routePolicy) error
	QuiesceTUN(context.Context) error
	CloseRoutes(context.Context) error
	ApplySysctls(context.Context, networkState, Config, Runtime) error
	RestoreSysctls(context.Context) error
	Close() error
}

type firewallState struct {
	Enabled        bool
	TCP            Mode
	UDP            Mode
	IPv6           bool
	TProxyMark     uint32
	TunMark        uint32
	TProxyPort     uint16
	TProxyTCP4     bool
	TProxyTCP6     bool
	TProxyUDP4     bool
	TProxyUDP6     bool
	Tun4           bool
	Tun6           bool
	TunName        string
	Local4         []string
	Local6         []string
	Gateway        []string
	BypassUIDs     []uint32
	BypassMarks    []uint32
	FakeIP         []netip.Prefix
	Bypass         []netip.Prefix
	BypassUDPPorts []uint16
}

type firewall interface {
	RecoverScopes(context.Context) ([]captureScope, bool, error)
	Apply(context.Context, firewallState) error
	Close(context.Context) error
}

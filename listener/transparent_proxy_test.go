package listener

import (
	"errors"
	"net"
	"net/netip"
	"reflect"
	"testing"

	"github.com/metacubex/mihomo/component/transparentproxy"
	C "github.com/metacubex/mihomo/constant"
	LC "github.com/metacubex/mihomo/listener/config"
)

type fakeTunListener struct {
	config   LC.Tun
	device   string
	index    int
	closed   int
	reloads  int
	onClose  func()
	closeErr error
}

func (l *fakeTunListener) Config() LC.Tun     { return l.config.Clone() }
func (l *fakeTunListener) Address() string    { return l.device }
func (l *fakeTunListener) DeviceName() string { return l.device }
func (l *fakeTunListener) DeviceIndex() int {
	if l.index != 0 {
		return l.index
	}
	return 42
}
func (l *fakeTunListener) OnReload() { l.reloads++ }
func (l *fakeTunListener) Close() error {
	l.closed++
	if l.onClose != nil {
		l.onClose()
	}
	return l.closeErr
}

type fakeTProxyListener struct {
	raw, bound string
	ipv4, ipv6 bool
	closed     int
}

func (l *fakeTProxyListener) RawAddress() string                { return l.raw }
func (l *fakeTProxyListener) Address() string                   { return l.bound }
func (l *fakeTProxyListener) Close() error                      { l.closed++; return nil }
func (l *fakeTProxyListener) TransparentFamilies() (bool, bool) { return l.ipv4, l.ipv6 }

func isolateTransparentListeners(t *testing.T) {
	t.Helper()
	oldTun, oldConf := tunLister, LastTunConf
	oldTCP, oldUDP, oldPort := tproxyListener, tproxyUDPListener, lastTProxyPort
	oldNewTun, oldNewTCP, oldNewUDP := newTunListener, newTProxyListener, newTProxyUDPListener
	oldBefore, oldNotify := beforeTunClose, notifyBackends
	oldLookup, oldAddresses := tunInterfaceByName, tunInterfaceAddresses
	oldConfig, oldIPv6 := transparentConfig, transparentIPv6Available
	oldAllowLAN, oldBind := allowLan, bindAddress
	t.Cleanup(func() {
		tunLister, LastTunConf = oldTun, oldConf
		tproxyListener, tproxyUDPListener, lastTProxyPort = oldTCP, oldUDP, oldPort
		newTunListener, newTProxyListener, newTProxyUDPListener = oldNewTun, oldNewTCP, oldNewUDP
		beforeTunClose, notifyBackends = oldBefore, oldNotify
		tunInterfaceByName, tunInterfaceAddresses = oldLookup, oldAddresses
		transparentConfig, transparentIPv6Available = oldConfig, oldIPv6
		allowLan, bindAddress = oldAllowLAN, oldBind
	})
	tunLister, LastTunConf = nil, LC.Tun{}
	tproxyListener, tproxyUDPListener, lastTProxyPort = nil, nil, 0
	allowLan, bindAddress = false, "*"
	beforeTunClose = func() error { return nil }
	notifyBackends = func(transparentproxy.BackendState) error { return nil }
	transparentConfig = transparentproxy.DefaultConfig
	transparentIPv6Available = func() bool { return false }
	tunInterfaceByName = func(name string) (*net.Interface, error) {
		if tunLister == nil {
			return nil, errTunDeviceMissing
		}
		return &net.Interface{Name: name, Index: 42, Flags: net.FlagUp}, nil
	}
	tunInterfaceAddresses = func(*net.Interface) ([]net.Addr, error) { return nil, nil }
	newTunListener = func(LC.Tun, C.Tunnel) (tunListener, error) { t.Fatal("unexpected TUN creation"); return nil, nil }
	newTProxyListener = func(string, C.Tunnel) (C.Listener, error) { t.Fatal("unexpected TCP creation"); return nil, nil }
	newTProxyUDPListener = func(string, C.Tunnel) (C.Listener, error) { t.Fatal("unexpected UDP creation"); return nil, nil }
}

func TestTunReloadRetainsStableRequestAndReusesDevice(t *testing.T) {
	isolateTransparentListeners(t)
	requested := LC.Tun{Enable: true, DNSHijack: []string{"b", "a"}}
	var live *fakeTunListener
	created, barriers := 0, 0
	newTunListener = func(options LC.Tun, _ C.Tunnel) (tunListener, error) {
		created++
		// Simulate listener normalization and a write into its own slices.
		options.Device, options.GSOMaxSize = "utun-generated", 65536
		options.DNSHijack[0] = "listener-owned"
		live = &fakeTunListener{config: options, device: options.Device}
		return live, nil
	}
	beforeTunClose = func() error { barriers++; return nil }
	for i := 0; i < 2; i++ {
		if err := ReCreateTun(requested, nil); err != nil {
			t.Fatal(err)
		}
	}
	if created != 1 || barriers != 0 || live.reloads != 1 || live.closed != 0 {
		t.Fatalf("equivalent reload recreated TUN: created=%d barriers=%d listener=%+v", created, barriers, live)
	}
	if requested.DNSHijack[0] != "b" || LastTunConf.DNSHijack[0] != "a" || LastTunConf.Device != "" {
		t.Fatal("sorting or listener defaults mutated the stable request")
	}
	snapshot := GetLastTunConf()
	snapshot.DNSHijack[0] = "external mutation"
	if GetLastTunConf().DNSHijack[0] != "a" {
		t.Fatal("request accessor exposed mutable state")
	}
}

func TestTunBlackholeFailurePreservesOldListenerAndRequest(t *testing.T) {
	isolateTransparentListeners(t)
	old := &fakeTunListener{config: LC.Tun{Enable: true, Device: "utun", MTU: 1500}, device: "utun"}
	tunLister, LastTunConf = old, old.config.Clone()
	wanted := old.config.Clone()
	wanted.MTU = 1400
	rejected := errors.New("route transaction failed")
	var published transparentproxy.BackendState
	beforeTunClose = func() error { return rejected }
	notifyBackends = func(state transparentproxy.BackendState) error { published = state; return nil }
	if err := ReCreateTun(wanted, nil); !errors.Is(err, rejected) {
		t.Fatalf("barrier failure was hidden: %v", err)
	}
	if tunLister != old || old.closed != 0 || !LastTunConf.Equal(old.config) || !GetTunConf().Enable {
		t.Fatal("failed guard changed or closed the old TUN")
	}
	if !published.TunReady || published.TunName != "utun" || published.TunIndex != 42 {
		t.Fatalf("failed guard did not republish the real old backend: %+v", published)
	}
	if err := Cleanup(); !errors.Is(err, rejected) || old.closed != 0 {
		t.Fatal("cleanup bypassed the same barrier")
	}

	var order []string
	beforeTunClose = func() error { order = append(order, "blackhole"); return nil }
	old.onClose = func() { order = append(order, "close") }
	newTunListener = func(cfg LC.Tun, _ C.Tunnel) (tunListener, error) {
		order = append(order, "create")
		return &fakeTunListener{config: cfg, device: "utun"}, nil
	}
	notifyBackends = func(state transparentproxy.BackendState) error {
		order = append(order, "publish")
		if !state.TunReady {
			t.Fatal("new listener was not ready at publication")
		}
		return nil
	}
	if err := ReCreateTun(wanted, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"blackhole", "close", "create", "publish"}) {
		t.Fatalf("unsafe replacement order: %v", order)
	}
}

func TestTunFailedConstructionCanRetrySameRequest(t *testing.T) {
	isolateTransparentListeners(t)
	wanted := LC.Tun{Enable: true, Device: "utun"}
	attempts := 0
	newTunListener = func(cfg LC.Tun, _ C.Tunnel) (tunListener, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("device unavailable")
		}
		return &fakeTunListener{config: cfg, device: "utun"}, nil
	}
	if err := ReCreateTun(wanted, nil); err == nil {
		t.Fatal("creation failure was hidden")
	}
	if GetTunConf().Enable || !GetLastTunConf().Enable {
		t.Fatal("actual readiness and retry intent were conflated")
	}
	if err := ReCreateTun(wanted, nil); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || !GetTunConf().Enable {
		t.Fatal("same enabled request did not retry")
	}
}

func TestTunIPv6RecoveryRetainsIntentAndUsesGuardedReplacement(t *testing.T) {
	isolateTransparentListeners(t)
	policy := transparentproxy.DefaultConfig()
	policy.Enable, policy.TCP = true, transparentproxy.ModeOff
	transparentConfig = func() transparentproxy.Config { return policy }
	available := false
	transparentIPv6Available = func() bool { return available }
	ipv6 := netip.MustParsePrefix("fdfe:dcba:9876::1/126")
	wanted := LC.Tun{Enable: true, Device: "utun", AutoRoute: true, AutoRedirect: true, Inet6Address: []netip.Prefix{ipv6}}
	var actualAddresses []net.Addr
	var created []*fakeTunListener
	barriers := 0
	beforeTunClose = func() error { barriers++; return nil }
	tunInterfaceAddresses = func(*net.Interface) ([]net.Addr, error) { return actualAddresses, nil }
	newTunListener = func(cfg LC.Tun, _ C.Tunnel) (tunListener, error) {
		actualAddresses = nil
		if len(cfg.Inet6Address) != 0 {
			actualAddresses = []net.Addr{&net.IPNet{IP: net.IP(ipv6.Addr().AsSlice()), Mask: net.CIDRMask(ipv6.Bits(), 128)}}
		}
		live := &fakeTunListener{config: cfg, device: "utun"}
		created = append(created, live)
		return live, nil
	}
	if err := ReCreateTun(wanted, nil); err != nil {
		t.Fatal(err)
	}
	if len(created[0].config.Inet6Address) != 0 || len(LastTunConf.Inet6Address) != 1 || LastTunConf.AutoRoute || LastTunConf.AutoRedirect {
		t.Fatal("temporary IPv4 fallback lost intent or retained competing route management")
	}
	if !wanted.AutoRoute || !wanted.AutoRedirect || len(wanted.Inet6Address) != 1 {
		t.Fatal("normalization mutated source values")
	}
	if TransparentProxyBackends().TunIPv6Ready {
		t.Fatal("IPv4-only listener was reported ready for IPv6")
	}
	available = true
	if err := RepairTransparentBackends(nil); err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || created[0].closed != 1 || barriers != 1 || !TransparentProxyBackends().TunIPv6Ready {
		t.Fatal("restored IPv6 did not perform one guarded backend replacement")
	}
	if err := RepairTransparentBackends(nil); err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || barriers != 1 {
		t.Fatal("healthy TUN was recreated by an unrelated network event")
	}
	actualAddresses = nil
	if err := RepairTransparentBackends(nil); err != nil {
		t.Fatal(err)
	}
	if len(created) != 3 || barriers != 2 || !TransparentProxyBackends().TunIPv6Ready {
		t.Fatal("lost kernel IPv6 address was not repaired")
	}
}

func TestTProxyRetriesMissingProtocolAndIPv6WithoutClosingHealthyTCP(t *testing.T) {
	isolateTransparentListeners(t)
	policy := transparentproxy.DefaultConfig()
	policy.Enable, policy.UDP = true, transparentproxy.ModeTProxy
	transparentConfig = func() transparentproxy.Config { return policy }
	transparentIPv6Available = func() bool { return true }
	tcpCreates, udpCreates := 0, 0
	var tcp, udp *fakeTProxyListener
	newTProxyListener = func(addr string, _ C.Tunnel) (C.Listener, error) {
		if addr != ":7895" {
			t.Fatalf("managed TProxy retained loopback-only binding: %s", addr)
		}
		tcpCreates++
		tcp = &fakeTProxyListener{raw: addr, bound: "[::]:7895", ipv4: true, ipv6: true}
		return tcp, nil
	}
	newTProxyUDPListener = func(addr string, _ C.Tunnel) (C.Listener, error) {
		udpCreates++
		if udpCreates == 1 {
			return nil, errors.New("UDP port occupied")
		}
		udp = &fakeTProxyListener{raw: addr, bound: "0.0.0.0:7895", ipv4: true, ipv6: udpCreates >= 3}
		return udp, nil
	}
	if err := ReCreateTProxy(7895, nil); err == nil {
		t.Fatal("UDP failure was hidden")
	}
	state := TransparentProxyBackends()
	if !state.TProxyTCP || !state.TProxyTCPIPv6 || state.TProxyUDP || state.TProxyPort != 7895 {
		t.Fatalf("requested port was mistaken for UDP readiness: %+v", state)
	}
	if err := ReCreateTProxy(7895, nil); err != nil {
		t.Fatal(err)
	}
	if tcpCreates != 1 || udpCreates != 2 || !TransparentProxyBackends().TProxyUDP {
		t.Fatal("identical TCP address suppressed the UDP retry")
	}
	oldUDP := udp
	if err := RepairTransparentBackends(nil); err != nil {
		t.Fatal(err)
	}
	if tcpCreates != 1 || tcp.closed != 0 || udpCreates != 3 || oldUDP.closed != 1 || !TransparentProxyBackends().TProxyUDPIPv6 {
		t.Fatal("IPv6 repair replaced a healthy protocol or did not retry the missing family")
	}
	if err := RepairTransparentBackends(nil); err != nil {
		t.Fatal(err)
	}
	if tcpCreates != 1 || udpCreates != 3 {
		t.Fatal("healthy TProxy was recreated")
	}
}

func TestTransparentBackendsUseKernelIdentityAndWildcardReadiness(t *testing.T) {
	isolateTransparentListeners(t)
	LastTunConf = LC.Tun{Enable: true, Device: "requested-name"}
	if TransparentProxyBackends().TunReady {
		t.Fatal("enabled YAML was reported as a live device")
	}
	tunLister = &fakeTunListener{config: LastTunConf, device: "actual-fd-name", index: 77}
	tunInterfaceByName = func(name string) (*net.Interface, error) {
		if name != "actual-fd-name" {
			t.Fatalf("looked up request instead of kernel name: %s", name)
		}
		return &net.Interface{Name: name, Index: 77, Flags: net.FlagUp}, nil
	}
	tproxyListener = &fakeTProxyListener{raw: "127.0.0.1:7895", bound: "127.0.0.1:7895"}
	state := TransparentProxyBackends()
	if !state.TunReady || state.TunIndex != 77 || state.TunName != "actual-fd-name" || state.TProxyTCP || state.TProxyTCPIPv6 {
		t.Fatalf("incorrect actual backend state: %+v", state)
	}
	tunInterfaceByName = func(string) (*net.Interface, error) { return nil, errors.New("device removed") }
	if TransparentProxyBackends().TunReady {
		t.Fatal("removed device remained ready")
	}
}

func TestTunRecoveryRefusesForeignReusedDeviceName(t *testing.T) {
	isolateTransparentListeners(t)
	policy := transparentproxy.DefaultConfig()
	policy.Enable, policy.TCP = true, transparentproxy.ModeOff
	transparentConfig = func() transparentproxy.Config { return policy }
	old := &fakeTunListener{config: LC.Tun{Enable: true, Device: "utun"}, device: "utun", index: 42}
	tunLister, LastTunConf = old, old.config.Clone()
	tunInterfaceByName = func(name string) (*net.Interface, error) {
		return &net.Interface{Name: name, Index: 99, Flags: net.FlagUp}, nil
	}
	beforeTunClose = func() error { t.Fatal("foreign device reached close barrier"); return nil }
	state := TransparentProxyBackends()
	if state.TunReady || state.TunIndex != 42 {
		t.Fatalf("foreign link became this listener's identity: %+v", state)
	}
	if err := RepairTransparentBackends(nil); err == nil {
		t.Fatal("recovery silently adopted an externally recreated device")
	}
	if err := Cleanup(); err == nil {
		t.Fatal("cleanup could alter a foreign interface through an old descriptor")
	}
	if old.closed != 0 || tunLister != old || !LastTunConf.Equal(old.config) {
		t.Fatal("identity refusal changed the old listener or requested config")
	}
}

func TestTunRecoveryRebuildsMissingDeviceDespiteStaleCleanupError(t *testing.T) {
	isolateTransparentListeners(t)
	policy := transparentproxy.DefaultConfig()
	policy.Enable, policy.TCP = true, transparentproxy.ModeOff
	transparentConfig = func() transparentproxy.Config { return policy }
	cleanupErr := errors.New("old link missing during address cleanup")
	old := &fakeTunListener{config: LC.Tun{Enable: true, Device: "utun"}, device: "utun", closeErr: cleanupErr}
	tunLister, LastTunConf = old, old.config.Clone()
	present := false
	tunInterfaceByName = func(name string) (*net.Interface, error) {
		if !present {
			return nil, errTunDeviceMissing
		}
		return &net.Interface{Name: name, Index: 43, Flags: net.FlagUp}, nil
	}
	var order []string
	beforeTunClose = func() error { order = append(order, "blackhole"); return nil }
	old.onClose = func() { order = append(order, "close stale fd") }
	newTunListener = func(cfg LC.Tun, _ C.Tunnel) (tunListener, error) {
		order = append(order, "create")
		present = true
		return &fakeTunListener{config: cfg, device: "utun", index: 43}, nil
	}
	if err := RepairTransparentBackends(nil); !errors.Is(err, cleanupErr) {
		t.Fatalf("stale descriptor cleanup error was hidden: %v", err)
	}
	if !reflect.DeepEqual(order, []string{"blackhole", "close stale fd", "create"}) || old.closed != 1 {
		t.Fatalf("missing device did not perform guarded recovery: %v", order)
	}
	if state := TransparentProxyBackends(); !state.TunReady || state.TunIndex != 43 {
		t.Fatalf("replacement device was not published: %+v", state)
	}
}

func TestTunIPv4AddressLossTriggersGuardedRecovery(t *testing.T) {
	isolateTransparentListeners(t)
	policy := transparentproxy.DefaultConfig()
	policy.Enable, policy.TCP = true, transparentproxy.ModeOff
	transparentConfig = func() transparentproxy.Config { return policy }
	ipv4 := netip.MustParsePrefix("198.18.0.1/30")
	wanted := LC.Tun{Enable: true, Device: "utun", Inet4Address: []netip.Prefix{ipv4}}
	var addresses []net.Addr
	tunInterfaceAddresses = func(*net.Interface) ([]net.Addr, error) { return addresses, nil }
	created, barriers := 0, 0
	newTunListener = func(cfg LC.Tun, _ C.Tunnel) (tunListener, error) {
		created++
		addresses = []net.Addr{&net.IPNet{IP: net.IP(ipv4.Addr().AsSlice()), Mask: net.CIDRMask(ipv4.Bits(), 32)}}
		return &fakeTunListener{config: cfg, device: "utun"}, nil
	}
	beforeTunClose = func() error { barriers++; return nil }
	if err := ReCreateTun(wanted, nil); err != nil {
		t.Fatal(err)
	}
	addresses = nil
	if TransparentProxyBackends().TunReady {
		t.Fatal("TUN without its configured address remained ready")
	}
	if err := RepairTransparentBackends(nil); err != nil {
		t.Fatal(err)
	}
	if created != 2 || barriers != 1 || !TransparentProxyBackends().TunReady {
		t.Fatal("lost IPv4 address did not restore through the pre-close barrier")
	}
}

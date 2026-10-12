package listener

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"

	"github.com/metacubex/mihomo/component/transparentproxy"
	C "github.com/metacubex/mihomo/constant"
	LC "github.com/metacubex/mihomo/listener/config"
	"github.com/metacubex/mihomo/listener/redir"
	"github.com/metacubex/mihomo/listener/sing_tun"
	"github.com/metacubex/mihomo/listener/tproxy"
	"github.com/metacubex/mihomo/log"
)

// Serialize changes to both managed backends and their publication. The
// manager never calls back into listeners while this lock is held.
var transparentListenersMu sync.Mutex

var lastTProxyPort, lastRedirPort int

type tunListener interface {
	Config() LC.Tun
	Address() string
	DeviceName() string
	DeviceIndex() int
	OnReload()
	Close() error
}

var (
	newRedirListener = func(addr string, tunnel C.Tunnel) (C.Listener, error) {
		var listener *redir.Listener
		var err error
		if transparentConfig().Uses(transparentproxy.ModeRedir) {
			listener, err = redir.NewManaged(addr, tunnel)
		} else {
			listener, err = redir.New(addr, tunnel)
		}
		if err != nil {
			return nil, err
		}
		return listener, nil
	}
	newRedirUDPListener = func(addr string, tunnel C.Tunnel) (C.Listener, error) {
		listener, err := tproxy.NewUDP(addr, tunnel)
		if err != nil {
			return nil, err
		}
		return listener, nil
	}
	newTunListener = func(cfg LC.Tun, tunnel C.Tunnel) (tunListener, error) {
		l, err := sing_tun.New(cfg, tunnel)
		if err != nil {
			return nil, err
		}
		return l, nil
	}
	newTProxyListener = func(addr string, tunnel C.Tunnel) (C.Listener, error) {
		var l *tproxy.Listener
		var err error
		if transparentConfig().Uses(transparentproxy.ModeTProxy) {
			l, err = tproxy.NewManaged(addr, tunnel)
		} else {
			l, err = tproxy.New(addr, tunnel)
		}
		if err != nil {
			return nil, err
		}
		return l, nil
	}
	newTProxyUDPListener = func(addr string, tunnel C.Tunnel) (C.Listener, error) {
		var l *tproxy.UDPListener
		var err error
		if transparentConfig().Uses(transparentproxy.ModeTProxy) {
			l, err = tproxy.NewManagedUDP(addr, tunnel)
		} else {
			l, err = tproxy.NewUDP(addr, tunnel)
		}
		if err != nil {
			return nil, err
		}
		return l, nil
	}
	beforeTunClose           = transparentproxy.BeforeTunClose
	beforeRedirClose         = transparentproxy.BeforeRedirClose
	notifyBackends           = transparentproxy.NotifyBackends
	tunInterfaceByName       = lookupTunInterface
	transparentConfig        = transparentproxy.CurrentConfig
	transparentIPv6Available = transparentproxy.IPv6Available
	tunInterfaceAddresses    = func(iface *net.Interface) ([]net.Addr, error) { return iface.Addrs() }
)

var errTunDeviceMissing = errors.New("TUN interface no longer exists")

func lookupTunInterface(name string) (*net.Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range interfaces {
		if iface.Name == name {
			return &iface, nil
		}
	}
	return nil, errTunDeviceMissing
}

func ReCreateTun(tunConf LC.Tun, tunnel C.Tunnel) (err error) {
	transparentListenersMu.Lock()
	defer transparentListenersMu.Unlock()
	defer func() {
		// Also restore the actual old backend after a failed barrier. No path
		// closes it unless BeforeTunClose completed successfully.
		err = errors.Join(err, notifyBackends(transparentBackendsLocked()))
		if err != nil {
			log.Errorln("Start TUN listening error: %s", err)
		}
	}()

	return recreateTunLocked(tunConf, tunnel, false)
}

func recreateTunLocked(tunConf LC.Tun, tunnel C.Tunnel, repair bool) error {
	tunConf = tunConf.Clone()
	managed := transparentConfig().Enable
	if managed {
		tunConf.AutoRoute = false
		tunConf.AutoRedirect = false
	}
	tunConf.Sort()
	if !repair && tunConf.Equal(LastTunConf) && (tunLister != nil || !tunConf.Enable) {
		if tunLister != nil {
			tunLister.OnReload()
		}
		return nil
	}
	if managed && tunConf.Enable && tunConf.FileDescriptor == 0 && tunConf.Device != "" {
		device, lookupErr := tunInterfaceByName(tunConf.Device)
		if lookupErr != nil && !errors.Is(lookupErr, errTunDeviceMissing) {
			return fmt.Errorf("verify target TUN identity: %w", lookupErr)
		}
		if device != nil && (tunLister == nil || tunLister.DeviceName() != device.Name || tunLister.DeviceIndex() != device.Index) {
			return fmt.Errorf("refuse to replace foreign TUN %q (ifindex %d)", device.Name, device.Index)
		}
	}

	missing := false
	if tunLister != nil {
		_, lookupErr := tunInterfaceByName(tunLister.DeviceName())
		missing = errors.Is(lookupErr, errTunDeviceMissing)
	}
	closeErr := closeTunListenerLocked()
	if closeErr != nil && (tunLister != nil || !missing) {
		return closeErr
	}
	LastTunConf = tunConf.Clone()
	if !tunConf.Enable {
		return closeErr
	}

	options := tunConf.Clone()
	if managed && !transparentIPv6Available() {
		// Keep the request intact. A future kernel IPv6 event retries the
		// complete request through this same guarded replacement path.
		options.Inet6Address = nil
	}
	lister, err := newTunListener(options, tunnel)
	if err != nil {
		return errors.Join(closeErr, err)
	}
	tunLister = lister
	log.Infoln("[TUN] Tun adapter listening at: %s", tunLister.Address())
	return closeErr
}

func closeTunListenerLocked() error {
	if tunLister == nil {
		return nil
	}
	if index := tunLister.DeviceIndex(); index > 0 {
		device, err := tunInterfaceByName(tunLister.DeviceName())
		if err != nil && !errors.Is(err, errTunDeviceMissing) {
			return fmt.Errorf("preserve current TUN: verify kernel identity: %w", err)
		}
		if device != nil && device.Index != index {
			return fmt.Errorf("refuse to close TUN %q: ifindex changed from %d to %d", device.Name, index, device.Index)
		}
	} else if transparentConfig().Enable {
		return fmt.Errorf("preserve current TUN: original kernel identity is unknown")
	}
	if err := beforeTunClose(); err != nil {
		return fmt.Errorf("preserve current TUN: install routing blackhole before close: %w", err)
	}
	err := tunLister.Close()
	tunLister = nil
	return err
}

func ReCreateTProxy(port int, tunnel C.Tunnel) (err error) {
	transparentListenersMu.Lock()
	defer transparentListenersMu.Unlock()
	defer func() {
		err = errors.Join(err, notifyBackends(transparentBackendsLocked()))
		if err != nil {
			log.Errorln("Start TProxy server error: %s", err)
		}
	}()
	lastTProxyPort = port
	return recreateTProxyLocked(port, tunnel, false)
}

func ReCreateRedir(port int, tunnel C.Tunnel) (err error) {
	transparentListenersMu.Lock()
	defer transparentListenersMu.Unlock()
	defer func() {
		err = errors.Join(err, notifyBackends(transparentBackendsLocked()))
		if err != nil {
			log.Errorln("Start Redir server error: %s", err)
		}
	}()
	lastRedirPort = port
	return recreateRedirLocked(port, tunnel, false)
}

func recreateRedirLocked(port int, tunnel C.Tunnel, repairIPv6 bool) (err error) {
	cfg := transparentConfig()
	addr := genAddr(bindAddress, port, allowLan)
	if cfg.Uses(transparentproxy.ModeRedir) {
		addr = net.JoinHostPort("", strconv.Itoa(port))
	}
	_, ipv6 := tproxyFamilies(redirListener)
	if redirListener != nil && (redirListener.RawAddress() != addr || repairIPv6 && !ipv6) {
		if err := closeRedirListenerLocked(); err != nil {
			return err
		}
	}
	// Preserve the existing manually configured UDP TProxy sidecar. Managed
	// UDP never selects this socket and always uses its requested backend.
	udpAddr := genAddr(bindAddress, port, allowLan)
	if redirUDPListener != nil && redirUDPListener.RawAddress() != udpAddr {
		err = redirUDPListener.Close()
		redirUDPListener = nil
	}
	if portIsZero(addr) {
		return err
	}
	if redirListener == nil {
		var listenErr error
		redirListener, listenErr = newRedirListener(addr, tunnel)
		if listenErr != nil {
			return errors.Join(err, listenErr)
		}
		log.Infoln("Redirect proxy listening at: %s", redirListener.Address())
	}
	if redirUDPListener == nil {
		var udpErr error
		redirUDPListener, udpErr = newRedirUDPListener(udpAddr, tunnel)
		if udpErr != nil {
			log.Warnln("Failed to start Redir UDP Listener: %s", udpErr)
		}
	}
	return err
}

func closeRedirListenerLocked() error {
	if redirListener == nil {
		return nil
	}
	if err := beforeRedirClose(); err != nil {
		return err
	}
	err := redirListener.Close()
	redirListener = nil
	return err
}

func recreateTProxyLocked(port int, tunnel C.Tunnel, repairIPv6 bool) (err error) {
	cfg := transparentConfig()
	addr := genAddr(bindAddress, port, allowLan)
	if cfg.Uses(transparentproxy.ModeTProxy) {
		// TProxy receives packets for their original destination addresses.
		// A loopback-only listener cannot serve managed gateway traffic.
		addr = net.JoinHostPort("", strconv.Itoa(port))
	}
	_, tcpIPv6 := tproxyFamilies(tproxyListener)
	_, udpIPv6 := tproxyFamilies(tproxyUDPListener)
	if tproxyListener != nil && (tproxyListener.RawAddress() != addr || (repairIPv6 && cfg.TCP == transparentproxy.ModeTProxy && !tcpIPv6)) {
		err = errors.Join(err, tproxyListener.Close())
		tproxyListener = nil
	}
	if tproxyUDPListener != nil && (tproxyUDPListener.RawAddress() != addr || (repairIPv6 && cfg.UDP == transparentproxy.ModeTProxy && !udpIPv6)) {
		err = errors.Join(err, tproxyUDPListener.Close())
		tproxyUDPListener = nil
	}
	if portIsZero(addr) {
		return err
	}

	// Retry each protocol independently: a live TCP listener must not hide an
	// earlier UDP bind failure on an otherwise identical reload.
	if tproxyListener == nil {
		var tcpErr error
		tproxyListener, tcpErr = newTProxyListener(addr, tunnel)
		if tcpErr != nil {
			err = errors.Join(err, fmt.Errorf("TProxy TCP: %w", tcpErr))
		} else {
			log.Infoln("TProxy TCP server listening at: %s", tproxyListener.Address())
		}
	}
	if tproxyUDPListener == nil {
		var udpErr error
		tproxyUDPListener, udpErr = newTProxyUDPListener(addr, tunnel)
		if udpErr != nil {
			err = errors.Join(err, fmt.Errorf("TProxy UDP: %w", udpErr))
		} else {
			log.Infoln("TProxy UDP server listening at: %s", tproxyUDPListener.Address())
		}
	}
	return err
}

// TransparentProxyBackends describes live sockets and the actual kernel TUN
// identity. It does not infer readiness from requested enable/port values.
func TransparentProxyBackends() transparentproxy.BackendState {
	transparentListenersMu.Lock()
	defer transparentListenersMu.Unlock()
	return transparentBackendsLocked()
}

func transparentBackendsLocked() transparentproxy.BackendState {
	state := transparentproxy.BackendState{TProxyPort: tproxyPortLocked(), RedirPort: listenerPort(redirListener)}
	state.TProxyTCP, state.TProxyTCPIPv6 = tproxyFamilies(tproxyListener)
	state.TProxyUDP, state.TProxyUDPIPv6 = tproxyFamilies(tproxyUDPListener)
	state.RedirTCP, state.RedirTCPIPv6 = tproxyFamilies(redirListener)
	if tunLister != nil {
		state.TunName = tunLister.DeviceName()
		state.TunIndex = tunLister.DeviceIndex()
		if iface, err := tunInterfaceByName(state.TunName); err == nil {
			state.TunReady = state.TunIndex > 0 && iface.Index == state.TunIndex && iface.Flags&net.FlagUp != 0
			options := tunLister.Config()
			if state.TunReady && len(options.Inet4Address) != 0 {
				state.TunReady = tunHasAddressLocked(iface, options.Inet4Address)
			}
			state.TunIPv6Ready = state.TunReady && tunHasAddressLocked(iface, options.Inet6Address)
		}
	}
	return state
}

func tproxyFamilies(l C.Listener) (bool, bool) {
	if families, ok := l.(interface{ TransparentFamilies() (bool, bool) }); ok {
		return families.TransparentFamilies()
	}
	return false, false
}

func tunHasAddressLocked(iface *net.Interface, requested []netip.Prefix) bool {
	if len(requested) == 0 {
		return false
	}
	addresses, err := tunInterfaceAddresses(iface)
	if err != nil {
		return false
	}
	for _, address := range addresses {
		actual, err := netip.ParsePrefix(address.String())
		if err != nil {
			continue
		}
		for _, prefix := range requested {
			if actual.Addr() == prefix.Addr() {
				return true
			}
		}
	}
	return false
}

// RepairTransparentBackends only repairs missing kernel capabilities. Healthy
// devices and listeners are retained, including across ordinary IPv6 prefix
// rotations. A TUN rebuild still requires the synchronous blackhole barrier.
func RepairTransparentBackends(tunnel C.Tunnel) (err error) {
	transparentListenersMu.Lock()
	defer transparentListenersMu.Unlock()
	cfg := transparentConfig()
	if !cfg.Enable {
		return nil
	}
	defer func() { err = errors.Join(err, notifyBackends(transparentBackendsLocked())) }()
	ipv6Available := transparentIPv6Available()
	if cfg.Uses(transparentproxy.ModeTProxy) && lastTProxyPort > 0 {
		err = recreateTProxyLocked(lastTProxyPort, tunnel, ipv6Available)
	}
	if cfg.Uses(transparentproxy.ModeRedir) && lastRedirPort > 0 {
		err = errors.Join(err, recreateRedirLocked(lastRedirPort, tunnel, ipv6Available))
	}
	if cfg.Uses(transparentproxy.ModeTUN) && LastTunConf.Enable {
		state := transparentBackendsLocked()
		needsIPv6 := ipv6Available && len(LastTunConf.Inet6Address) != 0
		if !state.TunReady || (needsIPv6 && !state.TunIPv6Ready) {
			err = errors.Join(err, recreateTunLocked(LastTunConf, tunnel, true))
		}
	}
	return err
}

func tproxyPortLocked() uint16 {
	for _, l := range []C.Listener{tproxyListener, tproxyUDPListener} {
		if port := listenerPort(l); port != 0 {
			return port
		}
	}
	return 0
}

func listenerPort(l C.Listener) uint16 {
	if l != nil {
		_, port, err := net.SplitHostPort(l.Address())
		if err == nil {
			value, _ := strconv.ParseUint(port, 10, 16)
			return uint16(value)
		}
	}
	return 0
}

func Cleanup() error {
	transparentListenersMu.Lock()
	defer transparentListenersMu.Unlock()
	err := errors.Join(closeTunListenerLocked(), closeRedirListenerLocked())
	return errors.Join(err, notifyBackends(transparentBackendsLocked()))
}

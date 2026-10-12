package transparentproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/metacubex/mihomo/log"
	"golang.org/x/exp/slices"
)

const operationTimeout = 10 * time.Second

var managed struct {
	sync.Mutex
	current       *manager
	config        Config
	handoff       *manager
	active        atomic.Bool
	ipv6Available atomic.Bool
}

// Repair requests are coalesced. The listener consumes them in its own worker;
// no manager operation calls back into a listener or holds its lifecycle lock.
var backendRepairs = make(chan struct{}, 1)

func BackendRepairRequests() <-chan struct{} { return backendRepairs }

type manager struct {
	mu              sync.Mutex
	system          routeSystem
	wall            firewall
	previous        *manager
	config          Config
	runtime         Runtime
	uids            []uint32
	marks           []uint32
	lastFirewall    firewallState
	hasFirewall     bool
	closed          bool
	closing         bool
	recovered       bool
	restartPrepared bool
	uncertain       bool
	ctx             context.Context
	cancel          context.CancelFunc
	events          <-chan systemEvent
	watch           io.Closer
	kick            chan struct{}
	done            chan struct{}
	started         bool
	retryNeeded     bool
	nextRepair      time.Time
}

// BeginApply activates the new capture policy using the listeners which really
// exist before a configuration reload. An unavailable backend starts closed.
// All required route protection is acknowledged before nftables starts capture.
func BeginApply(cfg Config, runtime Runtime) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	managed.Lock()
	defer managed.Unlock()
	if !cfg.Enable {
		if err := closeManagedLocked(true); err != nil {
			return err
		}
		managed.config = cfg.Clone()
		return nil
	}
	uids, err := resolveBypassUsers(cfg.Local.BypassUsers)
	if err != nil {
		return err
	}
	m := managed.current
	if m != nil && m.closing {
		return errors.New("transparent-proxy stop cleanup is still in progress")
	}
	created := m == nil
	if created {
		system, err := newRouteSystem()
		if err != nil {
			return err
		}
		wall, err := newFirewall()
		if err != nil {
			return errors.Join(err, system.Close())
		}
		m = makeManager(system, wall)
		m.previous = managed.handoff
		// Subscribe before the first snapshot. A change during initial setup
		// remains queued and is reconciled after activation.
		if err := m.subscribe(); err != nil {
			m.cancel()
			return errors.Join(err, system.Close())
		}
		managed.current = m
		// Socket controls check this at execution time and then acquire this
		// mutex. Publishing before scanning existing sockets closes the gap
		// between old connections and newly constructed Dialers.
		managed.active.Store(true)
	}

	m.mu.Lock()
	protection, err := protectProcessSockets(cfg)
	if err == nil {
		marks := uniqueMarks(append(slices.Clone(m.marks), protection.marks...))
		err = m.applyConfigurationLocked(cfg, cloneRuntime(runtime), uids, marks)
	}
	if err == nil {
		if protection != nil {
			protection.commit()
		}
		managed.config = m.config.Clone()
		managed.handoff = nil
		m.startLocked()
		if m.retryNeeded {
			m.requestSync()
		}
		m.mu.Unlock()
		return nil
	}
	if !created {
		if protection != nil {
			err = errors.Join(err, protection.rollback())
		}
		m.requestSync()
		m.mu.Unlock()
		return err
	}
	cleanupErr := m.rollbackInitialApplyLocked(cfg, runtime, uids, protection)
	m.mu.Unlock()
	return errors.Join(err, cleanupErr)
}

// rollbackInitialApplyLocked runs with both lifecycle locks held. Until recovery
// succeeds, this instance has not changed any working network resource. Keep
// earlier protection intact if its complete ownership could not be observed.
func (m *manager) rollbackInitialApplyLocked(cfg Config, runtime Runtime, uids []uint32, protection *socketProtection) error {
	var cleanupErr error
	if !m.recovered {
		m.closed = true
		m.cancel()
		if m.watch != nil {
			cleanupErr = m.watch.Close()
			m.watch = nil
		}
		cleanupErr = errors.Join(cleanupErr, m.system.Close())
	} else {
		// Once resources may have changed, remove capture before relaxing
		// routes or undoing socket protection.
		cleanupErr = m.closeLocked()
	}
	if m.closed || m.closing {
		if m.closed {
			managed.current = nil
			if m.recovered {
				managed.handoff = nil
			}
			managed.active.Store(false)
			managed.ipv6Available.Store(false)
		}
		// This instance never changed capture, or removed it successfully.
		// Release pinned descriptors even if route/sysctl cleanup must retry.
		if protection != nil {
			cleanupErr = errors.Join(cleanupErr, protection.rollback())
		}
	} else {
		// If removing capture failed, keep the manager and its blackholes.
		// Retrying cleanup later is safer than abandoning an active ruleset.
		if protection != nil {
			m.marks = uniqueMarks(append(slices.Clone(m.marks), protection.marks...))
			protection.commit()
		}
		m.config, m.runtime, m.uids = cfg.Clone(), cloneRuntime(runtime), slices.Clone(uids)
		m.runtime.TunReady = false
		m.runtime.TunIPv6Ready = false
		m.runtime.TProxyTCP, m.runtime.TProxyUDP = false, false
		m.runtime.TProxyTCPIPv6, m.runtime.TProxyUDPIPv6 = false, false
		m.runtime.RedirTCP, m.runtime.RedirTCPIPv6 = false, false
		managed.config = m.config.Clone()
		m.startLocked()
		m.requestSync()
	}
	return cleanupErr
}

func makeManager(system routeSystem, wall firewall) *manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &manager{system: system, wall: wall, ctx: ctx,
		cancel: cancel, kick: make(chan struct{}, 1), done: make(chan struct{}),
		marks: []uint32{DefaultOutboundMark}}
}

func (m *manager) subscribe() error {
	events, watch, err := m.system.Watch(m.ctx)
	if err != nil {
		return fmt.Errorf("subscribe to network changes: %w", err)
	}
	m.events, m.watch = events, watch
	if m.started {
		// A synchronous lifecycle apply can replace the subscription while
		// the worker is waiting on its old channel or a recovery timer.
		m.requestSync()
	}
	return nil
}

func (m *manager) startLocked() {
	if !m.started && !m.closed && !m.closing {
		m.started = true
		go m.run()
		if m.retryNeeded {
			m.requestSync()
		}
	}
}

// Configure publishes actual listeners after a reload, and protects sockets
// opened by inbound listeners during that reload before enabling their routes.
func Configure(runtime Runtime) error {
	managed.Lock()
	defer managed.Unlock()
	m := managed.current
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return errors.New("transparent-proxy is stopping")
	}
	m.runtime = cloneRuntime(runtime)
	protection, err := protectProcessSockets(m.config)
	if err != nil {
		m.requestSync()
		return err
	}
	marks := uniqueMarks(append(slices.Clone(m.marks), protection.marks...))
	if err := m.applyConfigurationLocked(m.config, m.runtime, m.uids, marks); err != nil {
		m.requestSync()
		return errors.Join(err, protection.rollback())
	}
	protection.commit()
	if m.retryNeeded {
		m.requestSync()
	}
	return nil
}

func NotifyBackends(state BackendState) error {
	managed.Lock()
	defer managed.Unlock()
	m := managed.current
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return nil
	}
	m.setBackendsLocked(state)
	err := m.reconcileLocked()
	if err != nil || m.retryNeeded {
		m.requestSync()
	}
	return err
}

func (m *manager) setBackendsLocked(state BackendState) {
	m.runtime.TunName, m.runtime.TunIndex, m.runtime.TunReady = state.TunName, state.TunIndex, state.TunReady
	m.runtime.TunIPv6Ready = state.TunIPv6Ready
	m.runtime.TProxyPort = state.TProxyPort
	m.runtime.TProxyTCP, m.runtime.TProxyUDP = state.TProxyTCP, state.TProxyUDP
	m.runtime.TProxyTCPIPv6, m.runtime.TProxyUDPIPv6 = state.TProxyTCPIPv6, state.TProxyUDPIPv6
	m.runtime.RedirPort = state.RedirPort
	m.runtime.RedirTCP, m.runtime.RedirTCPIPv6 = state.RedirTCP, state.RedirTCPIPv6
}

// BeforeRedirClose blocks every selected packet before replacing a REDIRECT
// listener. NAT chains alone cannot provide this fence: established sessions
// retain their conntrack translation and skip those chains entirely.
func BeforeRedirClose() error {
	managed.Lock()
	defer managed.Unlock()
	m := managed.current
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return errors.New("finish transparent-proxy stop cleanup before closing REDIRECT listener")
	}
	if m.hasFirewall {
		state := m.lastFirewall
		state.Redir4, state.Redir6 = false, false
		ctx, cancel := context.WithTimeout(m.ctx, operationTimeout)
		defer cancel()
		if err := m.wall.Apply(ctx, state); err != nil {
			return fmt.Errorf("preserve REDIRECT listener: block capture before close: %w", err)
		}
		m.lastFirewall = state
	}
	m.runtime.RedirTCP, m.runtime.RedirTCPIPv6 = false, false
	return nil
}

// BeforeTunClose is a synchronous fence, not an asynchronous request. Returning
// an error means the caller must keep the old TUN open. Even if nftables fails,
// route blackholes already prevent marked packets falling through to main.
func BeforeTunClose() error {
	managed.Lock()
	defer managed.Unlock()
	m := managed.current
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return errors.New("finish transparent-proxy stop cleanup before closing TUN")
	}
	ctx, cancel := context.WithTimeout(m.ctx, operationTimeout)
	defer cancel()
	if err := m.system.QuiesceTUN(ctx); err != nil {
		return fmt.Errorf("confirm TUN routing blackholes: %w", err)
	}
	m.runtime.TunReady = false
	if m.hasFirewall {
		state := m.lastFirewall
		state.Tun4, state.Tun6 = false, false
		if err := m.wall.Apply(ctx, state); err != nil {
			return fmt.Errorf("block capture while TUN is unavailable: %w", err)
		}
		m.lastFirewall = state
	}
	return nil
}

func IsEnabled() bool { return managed.active.Load() }

// This is the last complete namespace observation, not WAN reachability. A
// listener uses it to retry dual-stack construction after IPv6 is re-enabled.
func IPv6Available() bool { return managed.ipv6Available.Load() }

func CurrentConfig() Config {
	managed.Lock()
	defer managed.Unlock()
	return managed.config.Clone()
}

// OutboundMark preserves an explicit routing-mark, and registers its bypass
// before a socket can send. It never combines bits with an administrator mark.
func OutboundMark(mark uint32) (uint32, error) {
	managed.Lock()
	defer managed.Unlock()
	m := managed.current
	if m == nil {
		return mark, nil
	}
	if mark == 0 {
		mark = DefaultOutboundMark
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validateOutboundMark(m.config, mark); err != nil {
		return 0, err
	}
	if m.closing {
		// Capture has already been removed. A new background socket must not
		// rebuild it while a failed route/sysctl cleanup is being retried.
		return mark, nil
	}
	if !m.hasFirewall {
		return 0, errors.New("transparent-proxy capture initialization has not completed")
	}
	if slices.Contains(m.marks, mark) {
		return mark, nil
	}
	marks := uniqueMarks(append(slices.Clone(m.marks), mark))
	state := m.lastFirewall
	state.BypassMarks = marks
	ctx, cancel := context.WithTimeout(m.ctx, operationTimeout)
	defer cancel()
	if err := m.wall.Apply(ctx, state); err != nil {
		return 0, err
	}
	m.marks, m.lastFirewall = marks, state
	return mark, nil
}

func validateOutboundMark(cfg Config, mark uint32) error {
	if mark == RedirMark || cfg.Uses(ModeTProxy) && mark == cfg.Routing.TProxy.Mark || cfg.Uses(ModeTUN) && mark == cfg.Routing.TUN.Mark {
		return fmt.Errorf("outbound routing-mark %#x is also a transparent capture mark", mark)
	}
	return nil
}

func Close() error {
	managed.Lock()
	defer managed.Unlock()
	return closeManagedLocked(false)
}

// PrepareRestart blocks capture synchronously but keeps observation alive until
// listener cleanup succeeds. AbortRestart can therefore recover a failed close;
// only CompleteRestart relinquishes the manager for process replacement.
func PrepareRestart() error {
	managed.Lock()
	defer managed.Unlock()
	m := managed.current
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return errors.New("transparent-proxy is stopping")
	}
	ctx, cancel := context.WithTimeout(m.ctx, operationTimeout)
	defer cancel()
	if err := m.system.QuiesceTUN(ctx); err != nil {
		m.requestSync()
		return fmt.Errorf("preserve current process: cannot protect TUN for restart: %w", err)
	}
	if m.hasFirewall {
		state := blockedFirewall(m.lastFirewall)
		if err := m.wall.Apply(ctx, state); err != nil {
			m.requestSync()
			return fmt.Errorf("preserve current process: cannot protect capture for restart: %w", err)
		}
		m.lastFirewall = state
	} else {
		return errors.New("preserve current process: capture initialization has not completed")
	}
	m.restartPrepared = true
	return nil
}

// AbortRestart publishes the listeners which survived a failed cleanup. If
// restoration itself fails the running observer retains protection and retries.
func AbortRestart(state BackendState) error {
	managed.Lock()
	defer managed.Unlock()
	m := managed.current
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setBackendsLocked(state)
	m.restartPrepared = false
	err := m.reconcileLocked()
	if err != nil || m.retryNeeded {
		m.requestSync()
	}
	return err
}

// CompleteRestart follows successful listener cleanup, before canceling the
// running configuration. Remove retired policy identities while their ownership
// is still in memory, then restore this process's sysctl baseline. Only current
// working rules and blackholes cross exec; the replacement can recover them
// from its configuration and samples its own current sysctl baseline.
func CompleteRestart() error {
	managed.Lock()
	defer managed.Unlock()
	m := managed.current
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if !m.restartPrepared {
		m.mu.Unlock()
		return errors.New("refuse to relinquish unprepared restart protection")
	}
	ctx, cancel := context.WithTimeout(m.ctx, operationTimeout)
	network, err := m.system.Snapshot(ctx)
	if err != nil {
		cancel()
		m.mu.Unlock()
		return fmt.Errorf("read transparent-proxy network state before restart: %w", err)
	}
	policies, _ := desiredState(m.config, m.runtime, network, m.uids, m.marks)
	for i := range policies {
		policies[i].Ready = false
	}
	// A previous reload may have left failed retired-resource cleanup pending.
	// Those old marks need not appear in the current nft capture or YAML, so a
	// fresh manager cannot safely infer their ownership after we relinquish it.
	// Keep the current blackholes and guards while completing that cleanup.
	if err := m.system.PruneRoutes(ctx, policies); err != nil {
		cancel()
		m.mu.Unlock()
		return fmt.Errorf("retire transparent-proxy route policies before restart: %w", err)
	}
	err = m.system.RestoreSysctls(ctx)
	cancel()
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("restore transparent-proxy network settings before restart: %w", err)
	}
	m.closed = true
	m.cancel()
	var closeErr error
	if m.watch != nil {
		closeErr = m.watch.Close()
		m.watch = nil
	}
	closeErr = errors.Join(closeErr, m.system.Close())
	started := m.started
	managed.current, managed.handoff = nil, m
	managed.active.Store(false)
	m.mu.Unlock()
	if started {
		<-m.done
	}
	if closeErr != nil {
		// Working protection remains in the kernel. A descriptor close error
		// must not reopen forwarding or abandon the prepared replacement.
		log.Warnln("[Transparent Proxy] Restart protection retained; closing observer: %v", closeErr)
	}
	return nil
}

func closeManagedLocked(probePrevious bool) error {
	m := managed.current
	recovering := m == nil
	if m == nil {
		if !probePrevious && managed.handoff == nil || runtime.GOOS != "linux" {
			return nil
		}
		wall, err := newFirewall()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
		defer cancel()
		captures, present, err := wall.RecoverScopes(ctx)
		if err != nil {
			// Default-disabled, unprivileged proxy use must not require network
			// administration access just to look for an earlier managed table.
			if managed.handoff == nil && (errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)) {
				return nil
			}
			return err
		}
		if !present && managed.handoff == nil {
			return nil
		}
		system, err := newRouteSystem()
		if err != nil {
			return err
		}
		var configured []routePolicy
		if previous := managed.handoff; previous != nil {
			network, readErr := system.Snapshot(ctx)
			if readErr != nil {
				return errors.Join(readErr, system.Close())
			}
			configured, _ = desiredState(previous.config, previous.runtime, network, previous.uids, previous.marks)
		}
		if err := system.RecoverRoutes(ctx, configured, captures); err != nil {
			return errors.Join(err, system.Close())
		}
		m = makeManager(system, wall)
		m.recovered = true
		managed.current = m
	}
	m.mu.Lock()
	err := m.closeLocked()
	closed, started := m.closed, m.started
	m.mu.Unlock()
	if closed {
		managed.current = nil
		managed.handoff = nil
		managed.active.Store(false)
		managed.ipv6Available.Store(false)
		managed.config.Enable = false
		if started {
			<-m.done
		}
	} else if recovering {
		// Keep the in-memory ownership and lease until a later stop attempt
		// completes. There is no external journal to recover lost progress.
		managed.active.Store(false)
	}
	return err
}

func (m *manager) closeLocked() error {
	if m.closed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	if err := m.wall.Close(ctx); err != nil {
		return fmt.Errorf("keep route protection because removing capture failed: %w", err)
	}
	// Once capture is removed, no event or backend callback may enable it
	// again. Retain ownership in memory until every cleanup operation succeeds.
	m.closing = true
	m.cancel()
	var watchErr error
	if m.watch != nil {
		watchErr = m.watch.Close()
		m.watch = nil
	}
	err := errors.Join(m.system.CloseRoutes(ctx), m.system.RestoreSysctls(ctx))
	if err != nil {
		return errors.Join(err, watchErr)
	}
	m.closed = true
	return errors.Join(watchErr, m.system.Close())
}

func (m *manager) applyConfigurationLocked(cfg Config, runtime Runtime, uids, marks []uint32) error {
	if m.closing || m.closed {
		return errors.New("transparent-proxy is stopping")
	}
	wasUncertain := m.uncertain
	if m.uncertain {
		if m.watch != nil {
			_ = m.watch.Close()
			m.watch = nil
		}
		if err := m.subscribe(); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(m.ctx, operationTimeout)
	defer cancel()
	for _, mark := range marks {
		if err := validateOutboundMark(cfg, mark); err != nil {
			return err
		}
	}
	network, err := m.system.Snapshot(ctx)
	if err != nil {
		return err
	}
	managed.ipv6Available.Store(network.IPv6Available)
	policies, state := desiredState(cfg, runtime, network, uids, marks)
	if !m.recovered {
		captures, _, err := m.wall.RecoverScopes(ctx)
		if err != nil {
			return fmt.Errorf("inspect existing transparent-proxy capture: %w", err)
		}
		configured := slices.Clone(policies)
		if previous := m.previous; previous != nil {
			oldPolicies, _ := desiredState(previous.config, previous.runtime, network, previous.uids, previous.marks)
			configured = append(configured, oldPolicies...)
		}
		if err := m.system.RecoverRoutes(ctx, configured, captures); err != nil {
			return fmt.Errorf("recover existing transparent-proxy routes: %w", err)
		}
		m.recovered = true
		m.previous = nil
	}
	if m.restartPrepared {
		for i := range policies {
			policies[i].Ready = false
		}
		state = blockedFirewall(state)
	}
	if err := m.system.EnsureRoutes(ctx, policies); err != nil {
		return fmt.Errorf("prepare transparent proxy routes: %w", err)
	}
	if err := m.system.ApplySysctls(ctx, network, cfg, runtime); err != nil {
		return fmt.Errorf("prepare transparent proxy networking: %w", err)
	}
	if err := m.wall.Apply(ctx, state); err != nil {
		return err
	}
	m.config, m.runtime = cfg.Clone(), cloneRuntime(runtime)
	m.uids, m.marks = slices.Clone(uids), slices.Clone(marks)
	m.lastFirewall, m.hasFirewall, m.uncertain = state, true, false
	m.retryNeeded = false
	// The new policy is already active. A retired-resource cleanup failure is
	// retried without falsely reporting that the old policy is still active.
	if err := m.system.PruneRoutes(ctx, policies); err != nil {
		log.Warnln("[Transparent Proxy] Retired route cleanup will retry: %v", err)
		m.retryNeeded = true
	}
	if !m.restartPrepared && needsBackendRepair(cfg, runtime, network) {
		m.retryNeeded = true
		if !time.Now().Before(m.nextRepair) {
			select {
			case backendRepairs <- struct{}{}:
			default:
			}
			m.nextRepair = time.Now().Add(time.Second)
		}
	} else {
		m.nextRepair = time.Time{}
	}
	if wasUncertain {
		// Only a complete successful apply restores observation. A missing
		// backend or restart fence can still leave selected traffic protected.
		tcp, udp := appliedBackendAvailability(state)
		log.Infoln("[Transparent Proxy] Network synchronization restored; applied backend availability: TCP=%s; UDP=%s", tcp, udp)
	}
	return nil
}

func appliedBackendAvailability(state firewallState) (string, string) {
	if !state.Enabled {
		return "disabled", "disabled"
	}
	describe := func(mode Mode, ready4, ready6 bool) string {
		if mode == ModeOff {
			return "off"
		}
		ipv4, ipv6 := "protected", "protected"
		if ready4 {
			ipv4 = "ready"
		}
		if !state.IPv6 {
			ipv6 = "disabled"
		} else if ready6 {
			ipv6 = "ready"
		}
		return fmt.Sprintf("%s (IPv4=%s, IPv6=%s)", mode, ipv4, ipv6)
	}
	tcp4, tcp6 := state.TProxyTCP4, state.TProxyTCP6
	switch state.TCP {
	case ModeTUN:
		tcp4, tcp6 = state.Tun4, state.Tun6
	case ModeRedir:
		tcp4, tcp6 = state.Redir4, state.Redir6
	}
	udp4, udp6 := state.TProxyUDP4, state.TProxyUDP6
	if state.UDP == ModeTUN {
		udp4, udp6 = state.Tun4, state.Tun6
	}
	return describe(state.TCP, tcp4, tcp6), describe(state.UDP, udp4, udp6)
}

func blockedFirewall(state firewallState) firewallState {
	state.Redir4, state.Redir6 = false, false
	state.Tun4, state.Tun6 = false, false
	state.TProxyTCP4, state.TProxyTCP6 = false, false
	state.TProxyUDP4, state.TProxyUDP6 = false, false
	return state
}

func (m *manager) reconcileLocked() error {
	if m.closed || m.closing {
		return nil
	}
	return m.applyConfigurationLocked(m.config, m.runtime, m.uids, m.marks)
}

func (m *manager) requestSync() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// loseObservationLocked refuses to infer removals from a partial dump. A fresh
// subscription followed by complete snapshots is required to resume capture.
func (m *manager) loseObservationLocked(cause error) {
	m.uncertain = true
	err := m.fenceCaptureLocked()
	log.Warnln("[Transparent Proxy] Network observation interrupted; capture is protected while resubscribing: %v", errors.Join(cause, err))
}

// fenceCaptureLocked repairs protection without resuming capture or restarting
// recovery. Both adapters compare live state before writing, so notifications
// from the fence itself converge without further writes.
func (m *manager) fenceCaptureLocked() error {
	ctx, cancel := context.WithTimeout(m.ctx, operationTimeout)
	defer cancel()
	err := m.system.QuiesceTUN(ctx)
	if m.hasFirewall {
		state := blockedFirewall(m.lastFirewall)
		if applyErr := m.wall.Apply(ctx, state); applyErr != nil {
			err = errors.Join(err, applyErr)
		} else {
			m.lastFirewall = state
		}
	}
	return err
}

func cloneRuntime(runtime Runtime) Runtime {
	runtime.FakeIP = slices.Clone(runtime.FakeIP)
	return runtime
}

func uniqueMarks(marks []uint32) []uint32 {
	marks = append(marks, DefaultOutboundMark)
	slices.Sort(marks)
	return slices.Compact(marks)
}

package transparentproxy

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"golang.org/x/exp/slices"
)

type managerRecorder struct {
	mu      sync.Mutex
	entries []string
}

func (r *managerRecorder) add(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, value)
}
func (r *managerRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.entries)
}
func (r *managerRecorder) reset() { r.mu.Lock(); defer r.mu.Unlock(); r.entries = nil }

type managerTestSystem struct {
	recorder                               *managerRecorder
	network                                networkState
	ensureErr, quiesceErr                  error
	recoverErr, closeRoutesErr, restoreErr error
	snapshotErr, pruneErr                  error
	prunedPolicies                         []routePolicy
	watchMu                                sync.Mutex
	watches                                []chan systemEvent
}

func (s *managerTestSystem) RecoverRoutes(context.Context, []routePolicy, []captureScope) error {
	s.recorder.add("recover-routes")
	return s.recoverErr
}
func (s *managerTestSystem) Watch(context.Context) (<-chan systemEvent, io.Closer, error) {
	s.recorder.add("watch")
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	ch := make(chan systemEvent, 8)
	s.watches = append(s.watches, ch)
	return ch, managerTestCloser{}, nil
}
func (s *managerTestSystem) Snapshot(context.Context) (networkState, error) {
	s.recorder.add("snapshot")
	return s.network, s.snapshotErr
}
func (s *managerTestSystem) EnsureRoutes(context.Context, []routePolicy) error {
	s.recorder.add("ensure")
	return s.ensureErr
}
func (s *managerTestSystem) PruneRoutes(_ context.Context, policies []routePolicy) error {
	s.recorder.add("prune")
	s.prunedPolicies = slices.Clone(policies)
	return s.pruneErr
}
func (s *managerTestSystem) QuiesceTUN(context.Context) error {
	s.recorder.add("blackhole-ack")
	return s.quiesceErr
}
func (s *managerTestSystem) CloseRoutes(context.Context) error {
	s.recorder.add("close-routes")
	return s.closeRoutesErr
}
func (s *managerTestSystem) ApplySysctls(context.Context, networkState, Config, Runtime) error {
	s.recorder.add("sysctls")
	return nil
}
func (s *managerTestSystem) RestoreSysctls(context.Context) error {
	s.recorder.add("restore-sysctls")
	return s.restoreErr
}
func (s *managerTestSystem) Close() error { s.recorder.add("close-system"); return nil }

type managerTestCloser struct{}

func (managerTestCloser) Close() error { return nil }

type managerTestFirewall struct {
	recorder           *managerRecorder
	state              firewallState
	applyErr, closeErr error
	recoverErr         error
}

func (f *managerTestFirewall) RecoverScopes(context.Context) ([]captureScope, bool, error) {
	f.recorder.add("read-capture")
	return nil, false, f.recoverErr
}

func (f *managerTestFirewall) Apply(_ context.Context, state firewallState) error {
	if !state.Tun4 && !state.Tun6 {
		f.recorder.add("capture-tun-closed")
	} else {
		f.recorder.add("capture")
	}
	if f.applyErr == nil {
		f.state = state
	}
	return f.applyErr
}
func (f *managerTestFirewall) Close(context.Context) error {
	f.recorder.add("close-capture")
	if f.closeErr == nil {
		f.state.Enabled = false
	}
	return f.closeErr
}

func testManager(t *testing.T) (*manager, *managerTestSystem, *managerTestFirewall, *managerRecorder) {
	t.Helper()
	r := &managerRecorder{}
	s := &managerTestSystem{recorder: r, network: testNetwork()}
	f := &managerTestFirewall{recorder: r}
	m := makeManager(s, f)
	cfg := DefaultConfig()
	cfg.Enable = true
	if err := m.applyConfigurationLocked(cfg, testRuntime(), nil, []uint32{DefaultOutboundMark}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.cancel() })
	return m, s, f, r
}

func TestManagerRoutesBeforeCaptureAndRetiredRoutesAfter(t *testing.T) {
	_, _, _, r := testManager(t)
	want := []string{"snapshot", "read-capture", "recover-routes", "ensure", "sysctls", "capture", "prune"}
	if !slices.Equal(r.all(), want) {
		t.Fatalf("unsafe application order: %v", r.all())
	}
}

func TestManagerFailedNewPolicyRetainsAppliedConfiguration(t *testing.T) {
	m, s, f, r := testManager(t)
	oldCfg, oldState := m.config.Clone(), f.state
	newCfg := oldCfg.Clone()
	newCfg.Routing.TUN.Table = 104
	s.ensureErr = errors.New("foreign route")
	r.reset()
	if err := m.applyConfigurationLocked(newCfg, testRuntime(), nil, m.marks); err == nil {
		t.Fatal("activation succeeded despite foreign route")
	}
	if !reflect.DeepEqual(m.config, oldCfg) || !reflect.DeepEqual(f.state, oldState) {
		t.Fatal("failed preparation published an unapplied configuration")
	}
	if slices.Contains(r.all(), "capture") || slices.Contains(r.all(), "prune") {
		t.Fatalf("capture or destructive cleanup ran after failed protection: %v", r.all())
	}
}

func withPublicManager(t *testing.T, m *manager) {
	t.Helper()
	managed.Lock()
	if managed.current != nil {
		managed.Unlock()
		t.Fatal("test manager already active")
	}
	managed.current, managed.config = m, m.config.Clone()
	managed.active.Store(true)
	managed.Unlock()
	t.Cleanup(func() {
		managed.Lock()
		managed.current = nil
		managed.config = Config{}
		managed.handoff = nil
		managed.active.Store(false)
		managed.Unlock()
	})
}

func TestRestartRetainsCaptureAndBlackholesForNextProcess(t *testing.T) {
	m, _, f, r := testManager(t)
	withPublicManager(t, m)
	r.reset()
	if err := PrepareRestart(); err != nil {
		t.Fatal(err)
	}
	want := []string{"blackhole-ack", "capture-tun-closed"}
	if !slices.Equal(r.all(), want) {
		t.Fatalf("restart removed working protections: %v", r.all())
	}
	if f.state.Tun4 || f.state.Tun6 || f.state.TProxyTCP4 || f.state.TProxyTCP6 || f.state.TProxyUDP4 || f.state.TProxyUDP6 {
		t.Fatal("restart gap left a backend enabled")
	}
	if managed.current != m || !IsEnabled() || !m.restartPrepared {
		t.Fatal("restart relinquished observation before listener cleanup succeeded")
	}
	// Listener cleanup still goes through a live, synchronous route fence.
	r.reset()
	if err := BeforeTunClose(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.all(), want) {
		t.Fatal("listener cleanup skipped the live route fence")
	}
	r.reset()
	if err := CompleteRestart(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.all(), []string{"snapshot", "prune", "restore-sysctls", "close-system"}) || managed.handoff != m || managed.current != nil {
		t.Fatal("completed restart did not retain protection while relinquishing transport ownership")
	}
}

func TestRestartCompletesRetiredPolicyCleanupBeforeHandoff(t *testing.T) {
	m, s, _, r := testManager(t)
	oldPolicy := m.config.Routing.TUN
	cfg := m.config.Clone()
	cfg.Routing.TUN.Mark, cfg.Routing.TUN.Table, cfg.Routing.TUN.Priority = 0x163, 102, 1012
	s.pruneErr = errors.New("old policy cleanup temporarily failed")
	if err := m.applyConfigurationLocked(cfg, testRuntime(), nil, m.marks); err != nil || !m.retryNeeded {
		t.Fatalf("reload did not retain failed cleanup for retry: %v", err)
	}
	withPublicManager(t, m)
	if err := PrepareRestart(); err != nil {
		t.Fatal(err)
	}
	s.pruneErr, s.prunedPolicies = nil, nil
	r.reset()
	if err := CompleteRestart(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.all(), []string{"snapshot", "prune", "restore-sysctls", "close-system"}) {
		t.Fatalf("restart lost retired ownership before cleanup: %v", r.all())
	}
	if len(s.prunedPolicies) != 4 {
		t.Fatalf("restart discarded current dual-stack protection: %+v", s.prunedPolicies)
	}
	for _, policy := range s.prunedPolicies {
		wanted := cfg.Routing.TProxy
		if policy.Backend == ModeTUN {
			wanted = cfg.Routing.TUN
		} else if policy.Backend != ModeTProxy {
			t.Fatalf("restart introduced an external cleanup scope: %+v", policy)
		}
		if policy.Ready || policy.Mark != wanted.Mark || policy.Table != wanted.Table || policy.Priority != wanted.Priority || policy.Mark == oldPolicy.Mark {
			t.Fatalf("restart retained obsolete forwarding or broadened the keep scope: %+v", policy)
		}
	}
}

func TestRestartPolicyCleanupFailureRetainsOwnershipForRetry(t *testing.T) {
	for _, stage := range []string{"snapshot", "prune"} {
		t.Run(stage, func(t *testing.T) {
			m, s, _, r := testManager(t)
			withPublicManager(t, m)
			if err := m.subscribe(); err != nil {
				t.Fatal(err)
			}
			watch := m.watch
			if err := PrepareRestart(); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("temporary " + stage + " failure")
			want := []string{"snapshot"}
			if stage == "snapshot" {
				s.snapshotErr = failure
			} else {
				s.pruneErr = failure
				want = append(want, "prune")
			}
			r.reset()
			if err := CompleteRestart(); !errors.Is(err, failure) {
				t.Fatalf("restart hid pending policy cleanup: %v", err)
			}
			if !slices.Equal(r.all(), want) || managed.current != m || managed.handoff != nil || !IsEnabled() || m.closed || !m.restartPrepared || m.ctx.Err() != nil || m.watch != watch {
				t.Fatalf("failed cleanup relinquished ownership, observation or settings: %v", r.all())
			}
			s.snapshotErr, s.pruneErr = nil, nil
			r.reset()
			if err := CompleteRestart(); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(r.all(), []string{"snapshot", "prune", "restore-sysctls", "close-system"}) || managed.current != nil || managed.handoff != m || !m.closed || m.watch != nil {
				t.Fatalf("retry did not complete cleanup before handoff: %v", r.all())
			}
		})
	}
}

func TestRestartObservationStaysBlockedAndFailedCleanupCanRecover(t *testing.T) {
	m, s, f, r := testManager(t)
	withPublicManager(t, m)
	if err := PrepareRestart(); err != nil {
		t.Fatal(err)
	}
	actual := BackendState{TunName: "utun", TunIndex: 4, TunReady: true, TunIPv6Ready: true,
		TProxyPort: 7895, TProxyTCP: true, TProxyUDP: true, TProxyTCPIPv6: true, TProxyUDPIPv6: true}
	if err := NotifyBackends(actual); err != nil {
		t.Fatal(err)
	}
	if f.state.Tun4 || f.state.Tun6 || f.state.TProxyTCP4 || f.state.TProxyUDP6 {
		t.Fatal("listener notification reopened capture while restart was prepared")
	}
	// A cleanup identity error leaves the old listener intact. Abort restores
	// its actual state and retains observation rather than stranding traffic.
	if err := AbortRestart(actual); err != nil {
		t.Fatal(err)
	}
	if m.restartPrepared || managed.current != m || !IsEnabled() || !f.state.Tun4 || !f.state.Tun6 || !f.state.TProxyTCP4 {
		t.Fatal("failed restart cleanup did not resume the surviving backends")
	}
	r.reset()
	s.quiesceErr = errors.New("route protection lost before second attempt")
	if err := PrepareRestart(); err == nil {
		t.Fatal("second restart skipped route verification")
	}
	if !slices.Equal(r.all(), []string{"blackhole-ack"}) || m.closed || m.restartPrepared {
		t.Fatal("failed second restart abandoned the active manager")
	}
}

func TestBeforeTunCloseRequiresSynchronousAcknowledgement(t *testing.T) {
	m, s, f, r := testManager(t)
	withPublicManager(t, m)
	r.reset()
	s.quiesceErr = errors.New("route ACK failed")
	if err := BeforeTunClose(); err == nil {
		t.Fatal("TUN close allowed without blackhole acknowledgement")
	}
	if !m.runtime.TunReady || !slices.Equal(r.all(), []string{"blackhole-ack"}) {
		t.Fatalf("old runtime changed before barrier success: %v", r.all())
	}
	s.quiesceErr = nil
	r.reset()
	if err := BeforeTunClose(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.all(), []string{"blackhole-ack", "capture-tun-closed"}) || m.runtime.TunReady || f.state.Tun4 || f.state.Tun6 {
		t.Fatalf("TUN barrier did not close both families: %v %+v", r.all(), f.state)
	}
	if !f.state.TProxyTCP4 || !f.state.TProxyTCP6 {
		t.Fatal("TUN rebuild unnecessarily disabled ready TProxy TCP")
	}
}

func TestCaptureRemovalFailureRetainsAllRouteProtection(t *testing.T) {
	m, _, f, r := testManager(t)
	f.closeErr = errors.New("nft transaction failed")
	r.reset()
	if err := m.closeLocked(); err == nil {
		t.Fatal("cleanup succeeded despite live capture")
	}
	if m.closed || !slices.Equal(r.all(), []string{"close-capture"}) {
		t.Fatalf("route protection removed while capture remained: %v", r.all())
	}
	f.closeErr = nil
	r.reset()
	if err := m.closeLocked(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.all(), []string{"close-capture", "close-routes", "restore-sysctls", "close-system"}) {
		t.Fatalf("unsafe cleanup order: %v", r.all())
	}
}

func TestStopRetriesCleanupWithoutLosingMemoryOrRecreatingCapture(t *testing.T) {
	for _, failed := range []string{"routes", "sysctls"} {
		t.Run(failed, func(t *testing.T) {
			m, s, f, r := testManager(t)
			withPublicManager(t, m)
			failure := errors.New("temporary cleanup failure")
			if failed == "routes" {
				s.closeRoutesErr = failure
			} else {
				s.restoreErr = failure
			}
			r.reset()
			if err := Close(); !errors.Is(err, failure) {
				t.Fatalf("cleanup error lost: %v", err)
			}
			if managed.current != m || m.closed || !m.closing || f.state.Enabled || slices.Contains(r.all(), "close-system") {
				t.Fatalf("failed stop discarded ownership or left capture active: %v", r.all())
			}
			r.reset()
			if err := NotifyBackends(BackendState{TunReady: true, TunIPv6Ready: true}); err != nil {
				t.Fatal(err)
			}
			if err := m.reconcileLocked(); err != nil {
				t.Fatal(err)
			}
			if mark, err := OutboundMark(0x3344); err != nil || mark != 0x3344 {
				t.Fatalf("stopping socket control: %#x %v", mark, err)
			}
			if len(r.all()) != 0 {
				t.Fatalf("stop retry allowed capture to be recreated: %v", r.all())
			}
			if err := BeforeTunClose(); err == nil {
				t.Fatal("TUN closed before remaining stop cleanup finished")
			}
			if err := PrepareRestart(); err == nil {
				t.Fatal("restart replaced a pending stop")
			}
			s.closeRoutesErr, s.restoreErr = nil, nil
			if err := Close(); err != nil {
				t.Fatal(err)
			}
			if managed.current != nil || !m.closed || IsEnabled() {
				t.Fatal("successful retry did not finish stop")
			}
			want := []string{"close-capture", "close-routes", "restore-sysctls", "close-system"}
			if !slices.Equal(r.all(), want) {
				t.Fatalf("cleanup was not retried before releasing transports: %v", r.all())
			}
		})
	}
}

func TestRestartSettingsFailureKeepsObservationUntilAbort(t *testing.T) {
	m, s, f, r := testManager(t)
	withPublicManager(t, m)
	if err := PrepareRestart(); err != nil {
		t.Fatal(err)
	}
	s.restoreErr = errors.New("sysctl restore failed")
	r.reset()
	if err := CompleteRestart(); !errors.Is(err, s.restoreErr) {
		t.Fatalf("restart hid failed settings restoration: %v", err)
	}
	if managed.current != m || m.closed || !m.restartPrepared || !slices.Equal(r.all(), []string{"snapshot", "prune", "restore-sysctls"}) {
		t.Fatal("failed restart restoration relinquished its observer")
	}
	if f.state.Tun4 || f.state.Tun6 || f.state.TProxyTCP4 {
		t.Fatal("failed restoration reopened capture")
	}
	s.restoreErr = nil
	if err := AbortRestart(BackendState{TunName: "utun", TunIndex: 4, TunReady: true, TunIPv6Ready: true,
		TProxyPort: 7895, TProxyTCP: true, TProxyUDP: true, TProxyTCPIPv6: true, TProxyUDPIPv6: true}); err != nil {
		t.Fatal(err)
	}
	if m.restartPrepared || !f.state.Tun4 || !f.state.TProxyTCP4 {
		t.Fatal("aborted restart did not recover surviving backends")
	}
}

func TestInitialRecoveryFailureCannotPrepareRoutesOrCapture(t *testing.T) {
	r := &managerRecorder{}
	s := &managerTestSystem{recorder: r, network: testNetwork()}
	f := &managerTestFirewall{recorder: r, recoverErr: errors.New("incomplete nft snapshot")}
	m := makeManager(s, f)
	t.Cleanup(m.cancel)
	cfg := DefaultConfig()
	cfg.Enable = true
	if err := m.applyConfigurationLocked(cfg, testRuntime(), nil, nil); !errors.Is(err, f.recoverErr) {
		t.Fatalf("missing recovery error: %v", err)
	}
	if m.recovered || !slices.Equal(r.all(), []string{"snapshot", "read-capture"}) {
		t.Fatalf("incomplete observation authorized mutation: %v", r.all())
	}
	f.recoverErr = nil
	s.recoverErr = errors.New("incomplete route snapshot")
	r.reset()
	if err := m.applyConfigurationLocked(cfg, testRuntime(), nil, nil); !errors.Is(err, s.recoverErr) {
		t.Fatalf("missing route recovery error: %v", err)
	}
	if m.recovered || !slices.Equal(r.all(), []string{"snapshot", "read-capture", "recover-routes"}) {
		t.Fatalf("failed route recovery authorized mutation: %v", r.all())
	}
}

func TestInitialRecoveryFailurePreservesPreviousKernelProtection(t *testing.T) {
	r := &managerRecorder{}
	s := &managerTestSystem{recorder: r, network: testNetwork(), recoverErr: errors.New("incomplete route dump")}
	f := &managerTestFirewall{recorder: r, state: firewallState{Enabled: true}}
	m := makeManager(s, f)
	withPublicManager(t, m)
	previous := &manager{closed: true}
	managed.handoff = previous
	cfg := DefaultConfig()
	cfg.Enable = true
	if err := m.applyConfigurationLocked(cfg, testRuntime(), nil, nil); !errors.Is(err, s.recoverErr) {
		t.Fatalf("missing startup recovery failure: %v", err)
	}
	r.reset()
	protection := &socketProtection{finish: func(commit bool) error {
		if commit {
			t.Fatal("failed startup committed temporary socket protection")
		}
		r.add("rollback-sockets")
		return nil
	}}
	if err := m.rollbackInitialApplyLocked(cfg, testRuntime(), nil, protection); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.all(), []string{"close-system", "rollback-sockets"}) || !f.state.Enabled {
		t.Fatalf("failed observation destroyed old capture or route evidence: %v", r.all())
	}
	if managed.current != nil || managed.handoff != previous || IsEnabled() || !m.closed || protection.finish != nil {
		t.Fatal("failed startup retained descriptors or lost the previous same-process restart context")
	}
}

func TestInitialCleanupFailureReleasesSocketsAfterCaptureIsGone(t *testing.T) {
	m, s, f, r := testManager(t)
	withPublicManager(t, m)
	s.closeRoutesErr = errors.New("temporary route cleanup failure")
	r.reset()
	protection := &socketProtection{finish: func(commit bool) error {
		if commit {
			t.Fatal("removed capture committed temporary socket protection")
		}
		r.add("rollback-sockets")
		return nil
	}}
	if err := m.rollbackInitialApplyLocked(m.config, m.runtime, m.uids, protection); !errors.Is(err, s.closeRoutesErr) {
		t.Fatalf("cleanup failure was hidden: %v", err)
	}
	if !slices.Equal(r.all(), []string{"close-capture", "close-routes", "restore-sysctls", "rollback-sockets"}) {
		t.Fatalf("unsafe failed activation cleanup order: %v", r.all())
	}
	if managed.current != m || !m.closing || m.closed || f.state.Enabled || protection.finish != nil {
		t.Fatal("partial cleanup lost route ownership, retained capture or leaked pinned sockets")
	}
	s.closeRoutesErr = nil
	if err := Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitOutboundMarkRegisteredBeforeUse(t *testing.T) {
	m, _, f, r := testManager(t)
	withPublicManager(t, m)
	r.reset()
	const mark = uint32(0x12345678)
	got, err := OutboundMark(mark)
	if err != nil || got != mark || !slices.Contains(f.state.BypassMarks, mark) {
		t.Fatalf("explicit mark not preserved and registered: %#x %v %+v", got, err, f.state)
	}
	r.reset()
	if _, err := OutboundMark(mark); err != nil || len(r.all()) != 0 {
		t.Fatal("known mark caused redundant firewall transaction")
	}
	f.applyErr = errors.New("nft unavailable")
	if got, err := OutboundMark(mark + 1); err == nil || got != 0 {
		t.Fatal("unregistered mark allowed a socket to send")
	}
	if _, err := OutboundMark(m.config.Routing.TUN.Mark); err == nil {
		t.Fatal("outbound socket accepted capture mark")
	}
}

func TestUnconfirmedCaptureCannotAuthorizePreviouslyKnownMarks(t *testing.T) {
	m, _, _, r := testManager(t)
	m.hasFirewall = false
	m.marks = uniqueMarks(append(m.marks, 0x12345678))
	withPublicManager(t, m)
	r.reset()
	for _, mark := range []uint32{0, DefaultOutboundMark, 0x12345678} {
		if got, err := OutboundMark(mark); err == nil || got != 0 {
			t.Fatalf("unconfirmed capture authorized mark %#x: %#x, %v", mark, got, err)
		}
	}
	if len(r.all()) != 0 {
		t.Fatal("unconfirmed initialization attempted to use an incomplete firewall state")
	}
}

func TestManagerHealthyWorkerOnlyRunsOnEvents(t *testing.T) {
	m, s, _, r := testManager(t)
	if err := m.subscribe(); err != nil {
		t.Fatal(err)
	}
	r.reset()
	m.startLocked()
	// A bounded observation checks the absence of a periodic reconcile; this
	// is a lifecycle regression, not a performance benchmark.
	select {
	case <-m.done:
		t.Fatal("worker stopped unexpectedly")
	case <-time.After(120 * time.Millisecond):
	}
	if len(r.all()) != 0 {
		t.Fatalf("healthy worker polled without events: %v", r.all())
	}
	s.watchMu.Lock()
	events := s.watches[0]
	s.watchMu.Unlock()
	events <- systemEvent{}
	deadline := time.After(2 * time.Second)
	for !slices.Contains(r.all(), "prune") {
		select {
		case <-deadline:
			t.Fatal("network event did not reconcile")
		case <-time.After(time.Millisecond):
		}
	}
	m.cancel()
	<-m.done
}

func TestManagerObservationLossClosesCaptureBeforeFreshSnapshot(t *testing.T) {
	m, s, _, r := testManager(t)
	if err := m.subscribe(); err != nil {
		t.Fatal(err)
	}
	r.reset()
	m.startLocked()
	s.watchMu.Lock()
	events := s.watches[0]
	s.watchMu.Unlock()
	events <- systemEvent{Err: errors.New("ENOBUFS")}
	deadline := time.After(2 * time.Second)
	for !slices.Contains(r.all(), "prune") {
		select {
		case <-deadline:
			t.Fatalf("failed resubscription: %v", r.all())
		case <-time.After(time.Millisecond):
		}
	}
	m.cancel()
	<-m.done
	want := []string{"blackhole-ack", "capture-tun-closed", "watch", "snapshot", "ensure", "sysctls", "capture", "prune"}
	if !slices.Equal(r.all(), want) {
		t.Fatalf("unsafe recovery ordering: %v", r.all())
	}
}

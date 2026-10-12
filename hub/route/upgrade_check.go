package route

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/metacubex/mihomo/component/updater"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
	"github.com/metacubex/mihomo/hub/executor"
	"github.com/metacubex/mihomo/log"

	"github.com/metacubex/chi/render"
	"github.com/metacubex/http"
)

const (
	upgradeTaskLimit     = 10 * time.Minute
	upgradePhaseLimit    = 3 * time.Minute
	upgradeRestartNotice = 2 * time.Second
	upgradeRequestLimit  = 128
)

type upgradeCheckItem struct {
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	Name             string `json:"name"`
	Status           string `json:"status"`
	CurrentVersion   string `json:"current_version,omitempty"`
	LatestVersion    string `json:"latest_version,omitempty"`
	InstalledVersion string `json:"installed_version,omitempty"`
	PendingRestart   bool   `json:"pending_restart,omitempty"`
	AutoUpdate       *bool  `json:"auto_update,omitempty"`
	UpdateInterval   int64  `json:"update_interval,omitempty"`
	IntervalUnit     string `json:"interval_unit,omitempty"`
	NextCheck        string `json:"next_check,omitempty"`
	NextCheckInMS    *int64 `json:"next_check_in_ms,omitempty"`
	AutoUpdateState  string `json:"auto_update_state,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Error            string `json:"error,omitempty"`
}

type upgradeCheckSnapshot struct {
	InstanceID string             `json:"instance_id"`
	TaskID     string             `json:"task_id,omitempty"`
	RequestID  string             `json:"request_id,omitempty"`
	State      string             `json:"state"`
	StartedAt  string             `json:"started_at,omitempty"`
	FinishedAt string             `json:"finished_at,omitempty"`
	Items      []upgradeCheckItem `json:"items"`
}

type upgradeCheckStep struct {
	ids []string
	run func(context.Context, func(upgradeCheckItem))
}

type upgradeCheckPlan struct {
	ctx     context.Context
	items   []upgradeCheckItem
	steps   []upgradeCheckStep
	cleanup func()
}

type upgradeCheckManager struct {
	mu           sync.Mutex
	snapshot     upgradeCheckSnapshot
	plan         func() (*upgradeCheckPlan, error)
	idle         func() []upgradeCheckItem
	limit        time.Duration
	requests     map[string]string
	requestOrder []string
}

var errUpgradeConfigurationUnavailable = errors.New("configuration_unavailable")
var errUpgradeInstanceChanged = errors.New("instance_changed")
var errUpgradeRequestAlreadySeen = errors.New("request_already_seen")
var unifiedUpgrades = newUpgradeCheckManager(buildUpgradeCheckPlan, currentUpgradeCheckItems)

func newUpgradeCheckID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("cannot initialize update task identity")
	}
	return hex.EncodeToString(id[:])
}

func newUpgradeCheckManager(plan func() (*upgradeCheckPlan, error), idle func() []upgradeCheckItem) *upgradeCheckManager {
	return &upgradeCheckManager{snapshot: upgradeCheckSnapshot{InstanceID: newUpgradeCheckID(), State: "idle", Items: []upgradeCheckItem{}}, plan: plan, idle: idle, limit: upgradeTaskLimit, requests: make(map[string]string)}
}

func (m *upgradeCheckManager) snapshotLocked() upgradeCheckSnapshot {
	s := m.snapshot
	s.Items = append([]upgradeCheckItem{}, s.Items...)
	return s
}

func (m *upgradeCheckManager) status() upgradeCheckSnapshot {
	m.mu.Lock()
	s := m.snapshotLocked()
	m.mu.Unlock()
	return m.refreshPolicies(s)
}

func (m *upgradeCheckManager) refreshPolicies(s upgradeCheckSnapshot) upgradeCheckSnapshot {
	current := m.idle()
	if s.State == "idle" {
		s.Items = current
	} else {
		// Preserve the last task's results/latest remote versions, but show the
		// versions and YAML scheduling policy actually active now. A save/reload
		// or a later automatic update must not leave stale settings on screen.
		byID := make(map[string]upgradeCheckItem, len(current))
		for _, item := range current {
			byID[item.ID] = item
		}
		for i := range s.Items {
			item := &s.Items[i]
			if active, ok := byID[item.ID]; ok {
				item.AutoUpdate, item.UpdateInterval, item.IntervalUnit = active.AutoUpdate, active.UpdateInterval, active.IntervalUnit
				item.NextCheck, item.NextCheckInMS, item.AutoUpdateState = active.NextCheck, active.NextCheckInMS, active.AutoUpdateState
				if item.Kind == "kernel" || item.Kind == "ui" {
					item.CurrentVersion = active.CurrentVersion
				}
			} else {
				item.AutoUpdate, item.UpdateInterval, item.IntervalUnit = nil, 0, ""
				item.NextCheck, item.NextCheckInMS, item.AutoUpdateState = "", nil, ""
			}
		}
		seen := make(map[string]bool, len(s.Items))
		for _, item := range s.Items {
			seen[item.ID] = true
		}
		for _, item := range current {
			if !seen[item.ID] {
				s.Items = append(s.Items, item)
			}
		}
	}
	return s
}

func (m *upgradeCheckManager) start() (upgradeCheckSnapshot, error) {
	return m.startWithRequest("", "")
}

func (m *upgradeCheckManager) startWithRequest(requestID, instanceID string) (upgradeCheckSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Browser transports may replay a POST before receiving response bytes.
	// The instance check prevents that replay from starting a second update
	// after the first task has already replaced and restarted the process.
	if instanceID != "" && instanceID != m.snapshot.InstanceID {
		return upgradeCheckSnapshot{}, errUpgradeInstanceChanged
	}
	if requestID != "" {
		if taskID, seen := m.requests[requestID]; seen {
			if taskID != m.snapshot.TaskID {
				return upgradeCheckSnapshot{}, errUpgradeRequestAlreadySeen
			}
			return m.snapshotLocked(), nil
		}
	}
	if m.snapshot.State == "running" || m.snapshot.State == "restarting" {
		m.rememberRequestLocked(requestID)
		return m.snapshotLocked(), nil
	}
	plan, err := m.plan()
	if err != nil {
		return upgradeCheckSnapshot{}, err
	}
	m.snapshot.TaskID = newUpgradeCheckID()
	m.snapshot.RequestID = requestID
	m.snapshot.State, m.snapshot.StartedAt, m.snapshot.FinishedAt = "running", time.Now().UTC().Format(time.RFC3339), ""
	m.snapshot.Items = append([]upgradeCheckItem{}, plan.items...)
	m.rememberRequestLocked(requestID)
	s := m.snapshotLocked()
	go m.run(plan)
	return s, nil
}

func (m *upgradeCheckManager) rememberRequestLocked(requestID string) {
	if requestID == "" {
		return
	}
	// Keep only recent request identities, never resource sources or old task
	// snapshots. The window also includes callers joining an existing task.
	if len(m.requestOrder) == upgradeRequestLimit {
		delete(m.requests, m.requestOrder[0])
		m.requestOrder = m.requestOrder[1:]
	}
	m.requests[requestID] = m.snapshot.TaskID
	m.requestOrder = append(m.requestOrder, requestID)
}

func (m *upgradeCheckManager) report(item upgradeCheckItem) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if item.Kind == "geodata" && item.ID != "geodata" {
		for i, old := range m.snapshot.Items {
			if old.ID == "geodata" && old.Reason == "not_configured" {
				m.snapshot.Items = append(m.snapshot.Items[:i], m.snapshot.Items[i+1:]...)
				break
			}
		}
	}
	for i, old := range m.snapshot.Items {
		if old.ID == item.ID {
			m.snapshot.Items[i] = item
			if item.Status == "restarting" {
				m.snapshot.State = "restarting"
			}
			return
		}
	}
	if item.Kind == "geodata" {
		// A successfully refreshed classical provider can introduce a GeoData
		// dependency which did not exist when the unified task was started.
		m.snapshot.Items = append(m.snapshot.Items, item)
	}
}

func (m *upgradeCheckManager) setChecking(ids []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.snapshot.Items {
		for _, id := range ids {
			if m.snapshot.Items[i].ID == id {
				m.snapshot.Items[i].Status = "checking"
			}
		}
	}
}

func (m *upgradeCheckManager) run(plan *upgradeCheckPlan) {
	ctx, cancel := context.WithTimeout(plan.ctx, m.limit)
	defer cancel()
	func() {
		defer plan.cleanup()
		for _, step := range plan.steps {
			if ctx.Err() != nil {
				break
			}
			m.setChecking(step.ids)
			phase, finish := context.WithTimeout(ctx, upgradePhaseLimit)
			step.run(phase, m.report)
			finish()
		}
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot.State == "restarting" {
		// A real process restart never returns. Test/embedded restart bridges
		// can return while the replacement executable still awaits activation.
		return
	}
	m.snapshot.State = "completed"
	for i := range m.snapshot.Items {
		item := &m.snapshot.Items[i]
		if item.Status == "pending" || item.Status == "checking" {
			item.Status, item.Error = "error", upgradeCheckError(ctx.Err())
		}
		if item.Status == "error" {
			m.snapshot.State = "partial"
		}
	}
	m.snapshot.FinishedAt = time.Now().UTC().Format(time.RFC3339)
}

func getUpgradeCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	render.JSON(w, r, unifiedUpgrades.status())
}

func startUpgradeCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// Only the active YAML defines resource sources and destinations. An empty
	// request also prevents a caller from accidentally passing legacy options.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(body) != 0 || r.URL.RawQuery != "" {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, newError("invalid_request"))
		return
	}
	requestIDs := r.Header.Values("Idempotency-Key")
	instanceIDs := r.Header.Values("X-Mihomo-Instance-ID")
	var requestID, instanceID string
	if len(requestIDs) != 0 || len(instanceIDs) != 0 {
		if len(requestIDs) != 1 || len(instanceIDs) != 1 || !validUpgradeCheckID(requestIDs[0]) || !validUpgradeCheckID(instanceIDs[0]) {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, newError("invalid_request"))
			return
		}
		requestID, instanceID = requestIDs[0], instanceIDs[0]
	}
	snapshot, err := unifiedUpgrades.startWithRequest(requestID, instanceID)
	if err != nil {
		status, code := http.StatusConflict, upgradeCheckError(err)
		if errors.Is(err, errUpgradeConfigurationUnavailable) {
			status, code = http.StatusServiceUnavailable, "configuration_unavailable"
		}
		render.Status(r, status)
		render.JSON(w, r, newError(code))
		return
	}
	render.Status(r, http.StatusAccepted)
	// A joined/replayed task contains old scheduling delays. Refresh just this
	// accepted snapshot, preserving its request identity even if a newer task
	// starts concurrently after it completed.
	render.JSON(w, r, unifiedUpgrades.refreshPolicies(snapshot))
}

func validUpgradeCheckID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func upgradeCheckError(err error) string {
	switch {
	case errors.Is(err, errUpgradeInstanceChanged):
		return "instance_changed"
	case errors.Is(err, errUpgradeRequestAlreadySeen):
		return "request_already_seen"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled), errors.Is(err, executor.ErrUpdateConfigurationChanged), errors.Is(err, updater.ErrCoreConfigurationChanged), errors.Is(err, updater.ErrUIConfigurationChanged):
		return "configuration_changed"
	case errors.Is(err, updater.ErrCoreUpdateBusy), errors.Is(err, updater.ErrUIUpdateBusy), errors.Is(err, updater.ErrGetDatabaseUpdateSkip):
		return "busy"
	case errors.Is(err, updater.ErrCoreVersionUnavailable), errors.Is(err, updater.ErrUIVersionUnavailable):
		return "version_unavailable"
	default:
		return "update_failed"
	}
}

func boolPolicy(value bool) *bool { return &value }

type automaticScheduleSource interface {
	AutomaticSchedule() (time.Time, string)
}

func setUpgradeAutomaticSchedule(item *upgradeCheckItem, source automaticScheduleSource) {
	item.NextCheck, item.NextCheckInMS, item.AutoUpdateState = "", nil, ""
	if item.AutoUpdate == nil || !*item.AutoUpdate {
		return
	}
	item.AutoUpdateState = "unscheduled"
	if embedMode && (item.Kind == "kernel" || item.Kind == "geodata") {
		return
	}
	if source == nil || item.Reason == "not_configured" || item.Reason == "local_provider" || item.Reason == "embedded" {
		return
	}
	next, state := source.AutomaticSchedule()
	if state != "" {
		item.AutoUpdateState = state
	}
	if !next.IsZero() {
		item.NextCheck = next.UTC().Format(time.RFC3339Nano)
		delay := time.Until(next).Milliseconds()
		if delay < 0 {
			delay = 0
		}
		item.NextCheckInMS = &delay
	}
}

func upgradeCheckItems(cfg *executor.UpdateConfiguration) []upgradeCheckItem {
	if cfg != nil && cfg.Context != nil && cfg.Context.Err() != nil {
		cfg = nil
	}
	ui := updater.CurrentUIUpdater()
	if cfg != nil {
		ui = cfg.UI
	}
	uiStatus := ui.Status(context.Background(), false)
	items := []upgradeCheckItem{
		{ID: "kernel", Kind: "kernel", Name: "mihomo-optimized", Status: "pending", CurrentVersion: C.Version},
		{ID: "ui", Kind: "ui", Name: "mihomo", Status: "pending", CurrentVersion: uiStatus.CurrentVersion},
	}
	if cfg == nil {
		return items
	}
	general := cfg.General
	items[0].AutoUpdate, items[0].UpdateInterval, items[0].IntervalUnit = boolPolicy(general.CoreAutoUpdate), int64(general.CoreUpdateInterval), "hours"
	items[1].AutoUpdate, items[1].UpdateInterval, items[1].IntervalUnit = boolPolicy(general.ExternalUIAutoUpdate), int64(general.ExternalUIUpdateInterval), "hours"
	setUpgradeAutomaticSchedule(&items[0], &updater.DefaultCoreUpdater)
	setUpgradeAutomaticSchedule(&items[1], ui)
	geoNames := updater.GeoDatabasesForConfig(cfg.Geo)
	for _, name := range geoNames {
		item := upgradeCheckItem{ID: "geodata:" + name, Kind: "geodata", Name: name, Status: "pending", AutoUpdate: boolPolicy(general.GeoAutoUpdate), UpdateInterval: int64(general.GeoUpdateInterval), IntervalUnit: "hours"}
		setUpgradeAutomaticSchedule(&item, &updater.DefaultGeoUpdater)
		items = append(items, item)
	}
	if len(geoNames) == 0 {
		item := upgradeCheckItem{ID: "geodata", Kind: "geodata", Name: "GeoData", Status: "skipped", Reason: "not_configured", AutoUpdate: boolPolicy(general.GeoAutoUpdate), UpdateInterval: int64(general.GeoUpdateInterval), IntervalUnit: "hours"}
		setUpgradeAutomaticSchedule(&item, nil)
		items = append(items, item)
	}
	names := make([]string, 0, len(cfg.RuleProviders))
	for name := range cfg.RuleProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		provider := cfg.RuleProviders[name]
		item := upgradeCheckItem{ID: "rule-provider:" + name, Kind: "rule-provider", Name: name, Status: "pending"}
		if policy, ok := provider.(interface{ UpdateInterval() time.Duration }); ok {
			interval := policy.UpdateInterval()
			item.AutoUpdate, item.UpdateInterval, item.IntervalUnit = boolPolicy(interval > 0), int64(interval/time.Second), "seconds"
		}
		if provider.VehicleType() != P.HTTP {
			item.Status, item.Reason = "skipped", "local_provider"
		}
		if schedule, ok := provider.(automaticScheduleSource); ok {
			setUpgradeAutomaticSchedule(&item, schedule)
		}
		items = append(items, item)
	}
	if len(names) == 0 {
		items = append(items, upgradeCheckItem{ID: "rule-providers", Kind: "rule-provider", Name: "rule-providers", Status: "skipped", Reason: "not_configured"})
	}
	// ApplyConfig cancels this snapshot before replacing the global updater
	// workers. Do not combine its old YAML policy with a newly applied timer.
	if cfg.Context != nil && cfg.Context.Err() != nil {
		return upgradeCheckItems(nil)
	}
	return items
}

func currentUpgradeCheckItems() []upgradeCheckItem {
	return upgradeCheckItems(executor.CurrentUpdateConfiguration())
}

func buildUpgradeCheckPlan() (*upgradeCheckPlan, error) {
	cfg := executor.CurrentUpdateConfiguration()
	if cfg == nil || cfg.Context.Err() != nil {
		return nil, errUpgradeConfigurationUnavailable
	}
	reservation, err := updater.DefaultCoreUpdater.ReserveCheckAndUpdate()
	if err != nil {
		return nil, err
	}
	plan := &upgradeCheckPlan{ctx: cfg.Context, items: upgradeCheckItems(cfg), cleanup: reservation.Close}
	var coreItem, uiItem upgradeCheckItem
	var geoItems []upgradeCheckItem
	for _, item := range plan.items {
		item := item // Keep distinct provider closures with the Go 1.20 language version.
		switch item.Kind {
		case "kernel":
			coreItem = item
		case "ui":
			uiItem = item
		case "geodata":
			if item.Status == "pending" {
				geoItems = append(geoItems, item)
			}
		case "rule-provider":
			if item.Status != "pending" {
				continue
			}
			provider := cfg.RuleProviders[item.Name]
			plan.steps = append(plan.steps, upgradeCheckStep{ids: []string{item.ID}, run: func(ctx context.Context, report func(upgradeCheckItem)) {
				check, ok := provider.(interface {
					CheckAndUpdateWithCommit(context.Context, func(func() error) error) (bool, error)
				})
				if !ok {
					item.Status, item.Error = "error", "update_failed"
				} else {
					changed, err := check.CheckAndUpdateWithCommit(ctx, cfg.Commit)
					item.Status, item.Error = upgradeItemOutcome(changed, err)
				}
				report(item)
			}})
		}
	}
	if !embedMode {
		plan.steps = append(plan.steps, upgradeCheckStep{run: func(ctx context.Context, report func(upgradeCheckItem)) {
			geo := cfg.Geo
			if geo.UsageNow != nil {
				geo.Usage = geo.UsageNow()
				geo.UsageNow = nil // Pin one committed provider snapshot for this phase.
			}
			names := updater.GeoDatabasesForConfig(geo)
			active := make(map[string]bool, len(names))
			currentItems := make([]upgradeCheckItem, 0, len(names))
			for _, name := range names {
				active[name] = true
				item := upgradeCheckItem{ID: "geodata:" + name, Kind: "geodata", Name: name, Status: "checking", AutoUpdate: boolPolicy(cfg.General.GeoAutoUpdate), UpdateInterval: int64(cfg.General.GeoUpdateInterval), IntervalUnit: "hours"}
				currentItems = append(currentItems, item)
				report(item)
			}
			for _, previous := range geoItems {
				if !active[previous.Name] {
					previous.Status, previous.Reason = "skipped", "not_configured"
					report(previous)
				}
			}
			if len(names) == 0 {
				return
			}
			results, groupErr := updater.CheckAndUpdateGeoDatabasesContext(ctx, geo)
			for _, item := range currentItems {
				item.Status, item.Error = "error", upgradeCheckError(groupErr)
				for _, result := range results {
					if result.Name == item.Name {
						item.Status, item.Error = upgradeItemOutcome(result.Changed, result.Err)
						break
					}
				}
				report(item)
			}
		}})
	}
	plan.steps = append(plan.steps, upgradeCheckStep{ids: []string{uiItem.ID}, run: func(ctx context.Context, report func(upgradeCheckItem)) {
		result, err := cfg.UI.CheckAndUpdateContext(ctx)
		uiItem.CurrentVersion, uiItem.LatestVersion = result.CurrentVersion, result.LatestVersion
		uiItem.Status, uiItem.Error = upgradeItemOutcome(result.Updated, err)
		if result.Updated {
			uiItem.CurrentVersion = result.LatestVersion
		}
		report(uiItem)
	}})
	if !embedMode {
		plan.steps = append(plan.steps, upgradeCheckStep{ids: []string{coreItem.ID}, run: func(ctx context.Context, report func(upgradeCheckItem)) {
			executable, err := os.Executable()
			if err != nil {
				coreItem.Status, coreItem.Error = "error", "update_failed"
				report(coreItem)
				return
			}
			restart := func(path string, commit func() error) error {
				return executor.ApplyUpdateAndRestart(func() error {
					return cfg.Commit(commit)
				}, func() {
					coreItem.Status, coreItem.Error = "restarting", ""
					coreItem.InstalledVersion, coreItem.PendingRestart = coreItem.LatestVersion, true
					report(coreItem)
					// Pollers get a stable opportunity to record the expected new
					// version before this process replaces itself. No network work
					// or updater lock is held during this bounded notice period.
					time.Sleep(upgradeRestartNotice)
					restartProcess(path)
				})
			}
			result, err := reservation.CheckAndUpdate(ctx, executable, restart, func(result updater.CoreCheckResult) {
				coreItem.CurrentVersion, coreItem.LatestVersion = result.CurrentVersion, result.LatestVersion
				coreItem.Status = "checking"
				report(coreItem)
			})
			if result.Updated && err == nil {
				return
			}
			if result.Updated && err != nil {
				log.Errorln("Core file %s is installed but restart failed; this process still runs %s and needs a restart: %s", result.LatestVersion, result.CurrentVersion, err)
			} else if err != nil {
				log.Warnln("Core update failed: %s", err)
			}
			coreItem = coreUpgradeOutcome(coreItem, result, err)
			report(coreItem)
		}})
	} else {
		for i := range plan.items {
			if plan.items[i].Kind == "kernel" || plan.items[i].Kind == "geodata" && plan.items[i].Status == "pending" {
				plan.items[i].Status, plan.items[i].Reason = "skipped", "embedded"
			}
		}
	}
	return plan, nil
}

// Installed bytes and the running process have separate lifetimes. A failed
// restart must remain actionable instead of looking like a failed download or
// a successfully activated version. The executable installer retains its own
// pending-restart guard, so a later task cannot install another core over it.
func coreUpgradeOutcome(item upgradeCheckItem, result updater.CoreCheckResult, err error) upgradeCheckItem {
	item.CurrentVersion, item.LatestVersion = result.CurrentVersion, result.LatestVersion
	item.Status, item.Error = upgradeItemOutcome(result.Updated, err)
	if result.Updated {
		item.InstalledVersion, item.PendingRestart = result.LatestVersion, true
		if err != nil {
			item.Error = "installed_restart_failed"
		}
	}
	return item
}

func upgradeItemOutcome(changed bool, err error) (string, string) {
	if err != nil {
		return "error", upgradeCheckError(err)
	}
	if changed {
		return "updated", ""
	}
	return "up_to_date", ""
}

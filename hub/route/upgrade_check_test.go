package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/updater"
	"github.com/metacubex/mihomo/hub/executor"

	"github.com/metacubex/chi"
	"github.com/metacubex/http"
	"github.com/metacubex/http/httptest"
)

func waitUpgradeTask(t *testing.T, manager *upgradeCheckManager, state string) upgradeCheckSnapshot {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		snapshot := manager.status()
		if snapshot.State == state {
			return snapshot
		}
		select {
		case <-timeout.C:
			t.Fatalf("task did not become %s: %+v", state, snapshot)
		case <-tick.C:
		}
	}
}

func TestUnifiedUpgradePOSTSurvivesDisconnectAndJoinsRunningTask(t *testing.T) {
	previous := unifiedUpgrades
	t.Cleanup(func() { unifiedUpgrades = previous })
	entered, release, cleanup := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var plans atomic.Int64
	unifiedUpgrades = newUpgradeCheckManager(func() (*upgradeCheckPlan, error) {
		plans.Add(1)
		return &upgradeCheckPlan{ctx: context.Background(), items: []upgradeCheckItem{{ID: "ui", Kind: "ui", Status: "pending"}}, cleanup: func() { close(cleanup) }, steps: []upgradeCheckStep{{ids: []string{"ui"}, run: func(ctx context.Context, report func(upgradeCheckItem)) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				report(upgradeCheckItem{ID: "ui", Kind: "ui", Status: "error", Error: "task_lost"})
				return
			}
			report(upgradeCheckItem{ID: "ui", Kind: "ui", Status: "updated", CurrentVersion: "1.0.1"})
		}}}}, nil
	}, func() []upgradeCheckItem { return []upgradeCheckItem{} })
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/check", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	upgradeRouter().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("start status: %d %s", response.Code, response.Body.String())
	}
	var first upgradeCheckSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil || first.State != "running" || first.TaskID == "" || first.InstanceID == "" {
		t.Fatalf("invalid task identity: %+v %v", first, err)
	}
	<-entered
	cancel() // Closing the browser must not cancel the authorized task.
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		response := httptest.NewRecorder()
		upgradeRouter().ServeHTTP(response, httptest.NewRequest(method, "/check", nil))
		var current upgradeCheckSnapshot
		if err := json.Unmarshal(response.Body.Bytes(), &current); err != nil || current.TaskID != first.TaskID || current.InstanceID != first.InstanceID || plans.Load() != 1 {
			t.Fatalf("running task was duplicated/lost: %+v plans=%d %v", current, plans.Load(), err)
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("task response may be cached")
		}
	}
	close(release)
	snapshot := waitUpgradeTask(t, unifiedUpgrades, "completed")
	if snapshot.Items[0].Status != "updated" || snapshot.FinishedAt == "" {
		t.Fatalf("browser cancellation killed the task: %+v", snapshot)
	}
	<-cleanup
}

func TestUnifiedUpgradeGETIsLocalAndPOSTRejectsInputs(t *testing.T) {
	previous := unifiedUpgrades
	t.Cleanup(func() { unifiedUpgrades = previous })
	var starts atomic.Int64
	unifiedUpgrades = newUpgradeCheckManager(func() (*upgradeCheckPlan, error) {
		starts.Add(1)
		return nil, updater.ErrCoreUpdateBusy
	}, func() []upgradeCheckItem {
		return []upgradeCheckItem{{ID: "kernel", Kind: "kernel", Status: "pending", CurrentVersion: "mihomo-optimized-v1.0.3", AutoUpdate: boolPolicy(false), UpdateInterval: 12, IntervalUnit: "hours"}}
	})
	get := httptest.NewRecorder()
	upgradeRouter().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/check", nil))
	if get.Code != http.StatusOK || starts.Load() != 0 || !strings.Contains(get.Body.String(), `"auto_update":false`) || strings.Contains(get.Body.String(), `"items":null`) {
		t.Fatalf("GET did not return a local policy snapshot: %s", get.Body.String())
	}
	for _, test := range []struct{ path, body string }{
		{"/check", `{"url":"https://private.example/source?token=secret"}`},
		{"/check", "  {\"url\":\"elsewhere\"}"},
		{"/check", " "},
		{"/check?force=true", ""},
		{"/check?url=https://example.com", ""},
	} {
		response := httptest.NewRecorder()
		upgradeRouter().ServeHTTP(response, httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body)))
		if response.Code != http.StatusBadRequest || starts.Load() != 0 || strings.Contains(response.Body.String(), "token=") {
			t.Fatalf("accepted or disclosed forbidden input: %d %s", response.Code, response.Body.String())
		}
	}
	response := httptest.NewRecorder()
	upgradeRouter().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/check", nil))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"message":"busy"`) || starts.Load() != 1 {
		t.Fatalf("busy core was not reported safely: %d %s", response.Code, response.Body.String())
	}
}

func TestUnifiedUpgradeTaskOrderAndIndependentFailures(t *testing.T) {
	var order []string
	items := []upgradeCheckItem{{ID: "rule-provider:test", Status: "pending"}, {ID: "geodata:geosite", Status: "pending"}, {ID: "ui", Status: "pending"}, {ID: "kernel", Status: "pending"}}
	plan := &upgradeCheckPlan{ctx: context.Background(), items: items, cleanup: func() {}}
	for i, item := range items {
		i, item := i, item
		plan.steps = append(plan.steps, upgradeCheckStep{ids: []string{item.ID}, run: func(_ context.Context, report func(upgradeCheckItem)) {
			order = append(order, item.ID)
			item.Status = "updated"
			if i == 0 {
				item.Status, item.Error = "error", upgradeCheckError(errors.New("https://private.example/?secret=never-expose"))
			}
			report(item)
		}})
	}
	manager := newUpgradeCheckManager(func() (*upgradeCheckPlan, error) { return plan, nil }, func() []upgradeCheckItem { return nil })
	if _, err := manager.start(); err != nil {
		t.Fatal(err)
	}
	snapshot := waitUpgradeTask(t, manager, "partial")
	if strings.Join(order, ",") != "rule-provider:test,geodata:geosite,ui,kernel" || snapshot.Items[0].Error != "update_failed" || snapshot.Items[3].Status != "updated" {
		t.Fatalf("resource failure prevented later work or reordered core: %+v %v", snapshot, order)
	}
	data, _ := json.Marshal(snapshot)
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "private.example") {
		t.Fatal("task result exposed upstream credentials")
	}
}

func TestUnifiedUpgradeConfigurationCancellationAndTaskDeadline(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, cleanup := make(chan struct{}), make(chan struct{})
			var core atomic.Bool
			manager := newUpgradeCheckManager(func() (*upgradeCheckPlan, error) {
				return &upgradeCheckPlan{ctx: ctx, items: []upgradeCheckItem{{ID: "ui", Status: "pending"}, {ID: "kernel", Status: "pending"}}, cleanup: func() { close(cleanup) }, steps: []upgradeCheckStep{
					{ids: []string{"ui"}, run: func(ctx context.Context, report func(upgradeCheckItem)) {
						close(entered)
						<-ctx.Done()
						report(upgradeCheckItem{ID: "ui", Status: "error", Error: upgradeCheckError(ctx.Err())})
					}},
					{ids: []string{"kernel"}, run: func(context.Context, func(upgradeCheckItem)) { core.Store(true) }},
				}}, nil
			}, func() []upgradeCheckItem { return nil })
			if timeout {
				manager.limit = 20 * time.Millisecond
			}
			if _, err := manager.start(); err != nil {
				t.Fatal(err)
			}
			<-entered
			if !timeout {
				cancel()
			}
			snapshot := waitUpgradeTask(t, manager, "partial")
			want := "configuration_changed"
			if timeout {
				want = "timeout"
			}
			if core.Load() || snapshot.Items[1].Error != want || snapshot.Items[0].Error != want {
				t.Fatalf("canceled task continued installing core: %+v", snapshot)
			}
			<-cleanup
		})
	}
}

func TestUnifiedUpgradeRestartStatusSurvivesReturningBridge(t *testing.T) {
	manager := newUpgradeCheckManager(func() (*upgradeCheckPlan, error) {
		return &upgradeCheckPlan{ctx: context.Background(), items: []upgradeCheckItem{{ID: "kernel", Kind: "kernel", Status: "pending"}}, cleanup: func() {}, steps: []upgradeCheckStep{{ids: []string{"kernel"}, run: func(_ context.Context, report func(upgradeCheckItem)) {
			report(upgradeCheckItem{ID: "kernel", Kind: "kernel", Status: "restarting", LatestVersion: "mihomo-optimized-v1.0.4"})
		}}}}, nil
	}, func() []upgradeCheckItem { return nil })
	first, err := manager.start()
	if err != nil {
		t.Fatal(err)
	}
	_ = waitUpgradeTask(t, manager, "restarting")
	second, err := manager.start()
	if err != nil || second.TaskID != first.TaskID || second.Items[0].LatestVersion != "mihomo-optimized-v1.0.4" {
		t.Fatalf("pending restart accepted another task: %+v %v", second, err)
	}
	if newUpgradeCheckManager(nil, nil).snapshot.InstanceID == first.InstanceID {
		t.Fatal("process managers reused an instance identity")
	}
}

func TestUnifiedUpgradeStableErrorCodes(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{context.Canceled, "configuration_changed"},
		{executor.ErrUpdateConfigurationChanged, "configuration_changed"},
		{updater.ErrUIConfigurationChanged, "configuration_changed"},
		{updater.ErrCoreConfigurationChanged, "configuration_changed"},
		{context.DeadlineExceeded, "timeout"},
		{updater.ErrCoreUpdateBusy, "busy"},
		{updater.ErrUIUpdateBusy, "busy"},
		{updater.ErrGetDatabaseUpdateSkip, "busy"},
		{updater.ErrUIVersionUnavailable, "version_unavailable"},
		{updater.ErrCoreVersionUnavailable, "version_unavailable"},
	} {
		if got := upgradeCheckError(fmt.Errorf("wrapped: %w", test.err)); got != test.want {
			t.Fatalf("incorrect stable error %q for %v", got, test.err)
		}
	}
}

func TestUnifiedUpgradeGETRefreshesLocalVersionsAndPolicyWithoutChangingResult(t *testing.T) {
	current := upgradeCheckItem{ID: "ui", Kind: "ui", Status: "pending", CurrentVersion: "1.0.3", AutoUpdate: boolPolicy(false), UpdateInterval: 6, IntervalUnit: "hours"}
	manager := newUpgradeCheckManager(nil, func() []upgradeCheckItem { return []upgradeCheckItem{current} })
	manager.snapshot.State, manager.snapshot.TaskID = "completed", "old-task"
	manager.snapshot.Items = []upgradeCheckItem{{ID: "ui", Kind: "ui", Status: "updated", CurrentVersion: "1.0.1", LatestVersion: "1.0.1", AutoUpdate: boolPolicy(true), UpdateInterval: 24, IntervalUnit: "hours"}}
	snapshot := manager.status()
	item := snapshot.Items[0]
	if item.CurrentVersion != "1.0.3" || item.AutoUpdate == nil || *item.AutoUpdate || item.UpdateInterval != 6 || item.LatestVersion != "1.0.1" || item.Status != "updated" || snapshot.TaskID != "old-task" {
		t.Fatalf("GET lost results or retained obsolete local settings: %+v", snapshot)
	}
	if manager.snapshot.Items[0].CurrentVersion != "1.0.1" {
		t.Fatal("rendering a local snapshot mutated the completed task")
	}
}

func TestUnifiedUpgradeCanAddNewlyCommittedGeoDependencies(t *testing.T) {
	manager := newUpgradeCheckManager(nil, func() []upgradeCheckItem { return []upgradeCheckItem{} })
	manager.snapshot.State = "running"
	manager.snapshot.Items = []upgradeCheckItem{{ID: "kernel", Kind: "kernel", Status: "pending"}, {ID: "geodata", Kind: "geodata", Status: "skipped", Reason: "not_configured"}}
	before := manager.status()
	manager.report(upgradeCheckItem{ID: "geodata:geosite", Kind: "geodata", Name: "geosite", Status: "checking"})
	manager.report(upgradeCheckItem{ID: "geodata:geosite", Kind: "geodata", Name: "geosite", Status: "updated"})
	after := manager.status()
	if len(after.Items) != 2 || after.Items[1].ID != "geodata:geosite" || after.Items[1].Status != "updated" || before.Items[1].ID != "geodata" {
		t.Fatalf("new GeoData family was lost, duplicated, or mutated an earlier response: before=%+v after=%+v", before, after)
	}
}

func TestUnifiedUpgradeIdempotencyRetainsCompletedResultAndRejectsSupersededRequest(t *testing.T) {
	var plans atomic.Int64
	manager := newUpgradeCheckManager(func() (*upgradeCheckPlan, error) {
		plans.Add(1)
		return &upgradeCheckPlan{ctx: context.Background(), cleanup: func() {}}, nil
	}, func() []upgradeCheckItem { return []upgradeCheckItem{} })
	instance := manager.status().InstanceID
	firstKey, secondKey := newUpgradeCheckID(), newUpgradeCheckID()
	first, err := manager.startWithRequest(firstKey, instance)
	if err != nil || first.RequestID != firstKey {
		t.Fatalf("request identity was not retained: %+v %v", first, err)
	}
	completed := waitUpgradeTask(t, manager, "completed")
	replayed, err := manager.startWithRequest(firstKey, instance)
	if err != nil || replayed.TaskID != first.TaskID || replayed.State != "completed" || replayed.FinishedAt != completed.FinishedAt || plans.Load() != 1 {
		t.Fatalf("transport replay repeated a completed update: %+v plans=%d %v", replayed, plans.Load(), err)
	}
	second, err := manager.startWithRequest(secondKey, instance)
	if err != nil || second.TaskID == first.TaskID || second.RequestID != secondKey {
		t.Fatalf("a distinct user action did not start a new task: %+v %v", second, err)
	}
	_ = waitUpgradeTask(t, manager, "completed")
	if _, err := manager.startWithRequest(firstKey, instance); !errors.Is(err, errUpgradeRequestAlreadySeen) || plans.Load() != 2 {
		t.Fatalf("old request was mistaken for a new task: plans=%d %v", plans.Load(), err)
	}
	if current := manager.status(); current.TaskID != second.TaskID || current.RequestID != secondKey {
		t.Fatalf("rejected old request discarded the current result: %+v", current)
	}
	newInstance := newUpgradeCheckManager(func() (*upgradeCheckPlan, error) {
		t.Fatal("a POST replay crossed a process restart")
		return nil, nil
	}, func() []upgradeCheckItem { return []upgradeCheckItem{} })
	if _, err := newInstance.startWithRequest(firstKey, instance); !errors.Is(err, errUpgradeInstanceChanged) {
		t.Fatalf("restarted process accepted an obsolete instance: %v", err)
	}
}

func TestUnifiedUpgradeConcurrentRequestIdentitiesJoinAndReplayOneTask(t *testing.T) {
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var plans atomic.Int64
	manager := newUpgradeCheckManager(func() (*upgradeCheckPlan, error) {
		plans.Add(1)
		return &upgradeCheckPlan{ctx: context.Background(), cleanup: func() {}, steps: []upgradeCheckStep{{run: func(context.Context, func(upgradeCheckItem)) { <-release }}}}, nil
	}, func() []upgradeCheckItem { return []upgradeCheckItem{} })
	instance, initiatingKey := manager.status().InstanceID, newUpgradeCheckID()
	first, err := manager.startWithRequest(initiatingKey, instance)
	if err != nil {
		t.Fatal(err)
	}
	var callers sync.WaitGroup
	keys := make([]string, 32)
	for i := range keys {
		keys[i] = initiatingKey
		if i%2 == 0 {
			keys[i] = newUpgradeCheckID()
		}
	}
	for _, key := range keys {
		key := key
		callers.Add(1)
		go func() {
			defer callers.Done()
			snapshot, err := manager.startWithRequest(key, instance)
			if err != nil || snapshot.TaskID != first.TaskID || snapshot.RequestID != initiatingKey {
				t.Errorf("joining request changed the original identity: %+v %v", snapshot, err)
			}
		}()
	}
	callers.Wait()
	close(release)
	_ = waitUpgradeTask(t, manager, "completed")
	for _, key := range keys {
		if replay, err := manager.startWithRequest(key, instance); err != nil || replay.TaskID != first.TaskID || replay.State != "completed" {
			t.Fatalf("joining identity was not retained after completion: %+v %v", replay, err)
		}
	}
	if plans.Load() != 1 {
		t.Fatalf("concurrent or completed replay created %d plans", plans.Load())
	}
}

func TestUnifiedUpgradeRequestHistoryIsBounded(t *testing.T) {
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	manager := newUpgradeCheckManager(func() (*upgradeCheckPlan, error) {
		return &upgradeCheckPlan{ctx: context.Background(), cleanup: func() {}, steps: []upgradeCheckStep{{run: func(context.Context, func(upgradeCheckItem)) { <-release }}}}, nil
	}, func() []upgradeCheckItem { return []upgradeCheckItem{} })
	instance := manager.status().InstanceID
	for i := 1; i <= upgradeRequestLimit+1; i++ {
		if _, err := manager.startWithRequest(fmt.Sprintf("%032x", i), instance); err != nil {
			t.Fatal(err)
		}
	}
	manager.mu.Lock()
	if len(manager.requests) != upgradeRequestLimit || len(manager.requestOrder) != upgradeRequestLimit {
		t.Errorf("request history exceeded its bound: %d / %d", len(manager.requests), len(manager.requestOrder))
	}
	if _, retained := manager.requests[fmt.Sprintf("%032x", 1)]; retained {
		t.Error("oldest request identity was not evicted")
	}
	manager.mu.Unlock()
	close(release)
	_ = waitUpgradeTask(t, manager, "completed")
	if replay, err := manager.startWithRequest(fmt.Sprintf("%032x", 2), instance); err != nil || replay.State != "completed" {
		t.Fatalf("recent identity was evicted too early: %+v %v", replay, err)
	}
}

func TestUnifiedUpgradeRequestHeadersAreStrictAndCORSAllowsThem(t *testing.T) {
	previous := unifiedUpgrades
	t.Cleanup(func() { unifiedUpgrades = previous })
	var plans atomic.Int64
	unifiedUpgrades = newUpgradeCheckManager(func() (*upgradeCheckPlan, error) {
		plans.Add(1)
		return &upgradeCheckPlan{ctx: context.Background(), cleanup: func() {}}, nil
	}, func() []upgradeCheckItem { return []upgradeCheckItem{} })
	instance, key := unifiedUpgrades.status().InstanceID, newUpgradeCheckID()
	for _, test := range []struct {
		name            string
		keys, instances []string
	}{
		{"missing instance", []string{key}, nil},
		{"missing key", nil, []string{instance}},
		{"empty pair", []string{""}, []string{""}},
		{"uppercase", []string{strings.Repeat("A", 32)}, []string{instance}},
		{"invalid hex", []string{strings.Repeat("g", 32)}, []string{instance}},
		{"short key", []string{key[:31]}, []string{instance}},
		{"long key", []string{key + "0"}, []string{instance}},
		{"whitespace", []string{" " + key}, []string{instance}},
		{"duplicate key", []string{key, key}, []string{instance}},
		{"duplicate instance", []string{key}, []string{instance, instance}},
		{"invalid instance", []string{key}, []string{strings.Repeat("G", 32)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/check", nil)
			for _, value := range test.keys {
				request.Header.Add("Idempotency-Key", value)
			}
			for _, value := range test.instances {
				request.Header.Add("X-Mihomo-Instance-ID", value)
			}
			response := httptest.NewRecorder()
			upgradeRouter().ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || plans.Load() != 0 || !strings.Contains(response.Body.String(), `"message":"invalid_request"`) {
				t.Fatalf("invalid request headers started a task: %d %s", response.Code, response.Body.String())
			}
		})
	}
	request := httptest.NewRequest(http.MethodPost, "/check", nil)
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("X-Mihomo-Instance-ID", newUpgradeCheckID())
	response := httptest.NewRecorder()
	upgradeRouter().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || plans.Load() != 0 || !strings.Contains(response.Body.String(), `"message":"instance_changed"`) {
		t.Fatalf("obsolete process identity started a task: %d %s", response.Code, response.Body.String())
	}
	request.Header.Set("X-Mihomo-Instance-ID", instance)
	response = httptest.NewRecorder()
	upgradeRouter().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || plans.Load() != 1 || !strings.Contains(response.Body.String(), `"request_id":"`+key+`"`) {
		t.Fatalf("valid identity was not returned: %d %s", response.Code, response.Body.String())
	}
	_ = waitUpgradeTask(t, unifiedUpgrades, "completed")
	get := httptest.NewRecorder()
	upgradeRouter().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/check", nil))
	if !strings.Contains(get.Body.String(), `"request_id":"`+key+`"`) {
		t.Fatalf("GET cannot confirm the initiating action: %s", get.Body.String())
	}
	router := chi.NewRouter()
	Cors{AllowOrigins: []string{"https://dashboard.example"}}.Apply(router)
	router.Mount("/upgrade", upgradeRouter())
	preflight := httptest.NewRequest(http.MethodOptions, "/upgrade/check", nil)
	preflight.Header.Set("Origin", "https://dashboard.example")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflight.Header.Set("Access-Control-Request-Headers", "authorization, idempotency-key, x-mihomo-instance-id")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, preflight)
	allowed := strings.ToLower(response.Header().Get("Access-Control-Allow-Headers"))
	if response.Code < 200 || response.Code >= 300 || !strings.Contains(allowed, "idempotency-key") || !strings.Contains(allowed, "x-mihomo-instance-id") {
		t.Fatalf("CORS blocked protected update requests: %d %v", response.Code, response.Header())
	}
}

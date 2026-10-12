package route

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/metacubex/mihomo/config"
	"github.com/metacubex/mihomo/hub/executor"
)

type fixedAutomaticSchedule struct {
	next  time.Time
	state string
}

func (s fixedAutomaticSchedule) AutomaticSchedule() (time.Time, string) { return s.next, s.state }

func TestUpgradeSchedulePublishesActualDelayForEveryKind(t *testing.T) {
	for _, kind := range []string{"kernel", "ui", "geodata", "rule-provider"} {
		t.Run(kind, func(t *testing.T) {
			item := upgradeCheckItem{ID: kind, Kind: kind, AutoUpdate: boolPolicy(true)}
			next := time.Now().Add(time.Hour)
			setUpgradeAutomaticSchedule(&item, fixedAutomaticSchedule{next, "scheduled"})
			if item.NextCheck != next.UTC().Format(time.RFC3339Nano) || item.NextCheckInMS == nil || *item.NextCheckInMS <= (time.Hour-time.Second).Milliseconds() || *item.NextCheckInMS > time.Hour.Milliseconds() || item.AutoUpdateState != "scheduled" {
				t.Fatalf("live deadline was not serialized: %+v", item)
			}
			item.AutoUpdate = boolPolicy(false)
			setUpgradeAutomaticSchedule(&item, fixedAutomaticSchedule{next, "scheduled"})
			if item.NextCheck != "" || item.NextCheckInMS != nil || item.AutoUpdateState != "" {
				t.Fatal("disabled policy retained schedule metadata")
			}
		})
	}
}

func TestUpgradeScheduleDistinguishesDueRunningAndUnavailable(t *testing.T) {
	item := upgradeCheckItem{AutoUpdate: boolPolicy(true)}
	setUpgradeAutomaticSchedule(&item, fixedAutomaticSchedule{time.Now().Add(-time.Minute), "scheduled"})
	data, err := json.Marshal(item)
	if err != nil || !strings.Contains(string(data), `"next_check_in_ms":0`) {
		t.Fatal("an already-due timer omitted its zero remaining delay", string(data), err)
	}
	setUpgradeAutomaticSchedule(&item, fixedAutomaticSchedule{state: "checking"})
	if item.NextCheckInMS != nil || item.NextCheck != "" || item.AutoUpdateState != "checking" {
		t.Fatal("a running provider fabricated its next check before completion")
	}
	setUpgradeAutomaticSchedule(&item, nil)
	if item.AutoUpdateState != "unscheduled" || item.NextCheckInMS != nil {
		t.Fatal("an enabled policy without a worker invented a deadline")
	}
	for _, reason := range []string{"local_provider", "not_configured", "embedded"} {
		item.Reason = reason
		setUpgradeAutomaticSchedule(&item, fixedAutomaticSchedule{time.Now().Add(time.Hour), "scheduled"})
		if item.NextCheckInMS != nil || item.AutoUpdateState != "unscheduled" {
			t.Fatalf("%s incorrectly exposed a remote schedule", reason)
		}
	}
}

func TestUpgradeScheduleRefreshesCompletedTasksAndNewProviders(t *testing.T) {
	current := []upgradeCheckItem{{ID: "ui", Kind: "ui", AutoUpdate: boolPolicy(true)}}
	manager := newUpgradeCheckManager(nil, func() []upgradeCheckItem {
		setUpgradeAutomaticSchedule(&current[0], fixedAutomaticSchedule{time.Now().Add(time.Hour), "scheduled"})
		return current
	})
	manager.snapshot.State, manager.snapshot.TaskID = "completed", "previous-task"
	manager.snapshot.Items = []upgradeCheckItem{
		{ID: "ui", Kind: "ui", Status: "updated", NextCheck: "2000-01-01T00:00:00Z"},
		{ID: "rule-provider:removed", Kind: "rule-provider", Status: "updated", AutoUpdate: boolPolicy(true), NextCheck: "2000-01-01T00:00:00Z", AutoUpdateState: "scheduled"},
	}
	current = append(current, upgradeCheckItem{ID: "rule-provider:new", Kind: "rule-provider", Status: "pending", AutoUpdate: boolPolicy(true)})
	setUpgradeAutomaticSchedule(&current[1], fixedAutomaticSchedule{time.Now().Add(2 * time.Hour), "scheduled"})
	snapshot := manager.status()
	if len(snapshot.Items) != 3 || snapshot.Items[0].Status != "updated" || snapshot.Items[0].NextCheckInMS == nil || snapshot.Items[1].NextCheck != "" || snapshot.Items[1].AutoUpdateState != "" || snapshot.Items[2].NextCheckInMS == nil {
		t.Fatalf("latest schedules were lost behind a completed manual task: %+v", snapshot.Items)
	}
	if manager.snapshot.Items[0].NextCheck != "2000-01-01T00:00:00Z" {
		t.Fatal("status rendering mutated stored task history")
	}
}

func TestUpgradeScheduleRefreshesReplayedSnapshotWithoutChangingTaskIdentity(t *testing.T) {
	current := upgradeCheckItem{ID: "ui", Kind: "ui", AutoUpdate: boolPolicy(true)}
	setUpgradeAutomaticSchedule(&current, fixedAutomaticSchedule{time.Now().Add(time.Minute), "scheduled"})
	manager := newUpgradeCheckManager(nil, func() []upgradeCheckItem { return []upgradeCheckItem{current} })
	oldDelay := time.Hour.Milliseconds()
	accepted := upgradeCheckSnapshot{TaskID: "accepted", RequestID: "accepted-request", State: "completed", Items: []upgradeCheckItem{{ID: "ui", Kind: "ui", Status: "updated", NextCheckInMS: &oldDelay}}}
	manager.snapshot.TaskID, manager.snapshot.RequestID = "newer", "different-request"
	refreshed := manager.refreshPolicies(accepted)
	if refreshed.TaskID != accepted.TaskID || refreshed.RequestID != accepted.RequestID || refreshed.Items[0].Status != "updated" || refreshed.Items[0].NextCheckInMS == nil || *refreshed.Items[0].NextCheckInMS > time.Minute.Milliseconds() {
		t.Fatalf("replayed response lost identity or restarted an obsolete delay: %+v", refreshed)
	}
}

func TestUpgradeScheduleDoesNotMixCanceledConfigurationWithNewTimers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	items := upgradeCheckItems(&executor.UpdateConfiguration{
		Context: ctx,
		General: config.General{CoreAutoUpdate: true, CoreUpdateInterval: 24, ExternalUIAutoUpdate: true, ExternalUIUpdateInterval: 24, GeoAutoUpdate: true, GeoUpdateInterval: 24},
	})
	if len(items) != 2 {
		t.Fatalf("canceled configuration retained resources: %+v", items)
	}
	for _, item := range items {
		if item.AutoUpdate != nil || item.UpdateInterval != 0 || item.NextCheck != "" || item.NextCheckInMS != nil || item.AutoUpdateState != "" {
			t.Fatalf("old policy was paired with a possibly replaced global timer: %+v", item)
		}
	}
}

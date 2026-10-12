package updater

import (
	"context"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/geodata"
)

func TestAutomaticScheduleExposesLiveStateAndRetainsDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deadline := time.Now().Add(6 * time.Hour)
	coreJob := &coreAutomaticUpdate{ctx: ctx}
	geoJob := &geoAutomaticUpdate{ctx: ctx}
	uiJob := &uiAutomaticUpdate{ctx: ctx}
	coreJob.next.Store(&deadline)
	geoJob.next.Store(&deadline)
	uiJob.next.Store(&deadline)
	core := &CoreUpdater{automatic: coreJob}
	geo := &GeoUpdater{automatic: geoJob}
	ui := &UIUpdater{automatic: uiJob}
	for _, phase := range []struct {
		value uint32
		name  string
	}{{automaticScheduled, "scheduled"}, {automaticWaiting, "waiting"}, {automaticChecking, "checking"}, {automaticRetrying, "retrying"}} {
		coreJob.phase.Store(phase.value)
		geoJob.phase.Store(phase.value)
		uiJob.phase.Store(phase.value)
		for _, read := range []func() (time.Time, string){core.AutomaticSchedule, geo.AutomaticSchedule, ui.AutomaticSchedule} {
			next, state := read()
			if next != deadline || state != phase.name {
				t.Fatalf("schedule lost its monotonic deadline or state: %v / %s", next, state)
			}
		}
	}
	cancel()
	for _, read := range []func() (time.Time, string){core.AutomaticSchedule, geo.AutomaticSchedule, ui.AutomaticSchedule} {
		if next, state := read(); !next.IsZero() || state != "" {
			t.Fatal("canceled automatic job retained a schedule")
		}
	}
}

func TestAutomaticScheduleReportsReadinessWaitWithoutPostponingTimer(t *testing.T) {
	geoCheckFixture(t)
	ConfigureGeoUpdates(GeoUpdateConfig{Usage: geodata.Usage{GeoSite: true}})
	clock := &uiTestAutomaticClock{}
	ready, started, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	u := &GeoUpdater{newTicker: clock.newTicker, check: func(ctx context.Context) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}
	t.Cleanup(func() { stopGeoAutomatic(t, u) })
	u.ConfigureAutoUpdateWhenReady(true, 6, ready)
	first, state := u.AutomaticSchedule()
	if first.IsZero() || state != "waiting" {
		t.Fatalf("startup wait = %s / %s", first, state)
	}
	u.ConfigureAutoUpdateWhenReady(true, 6, ready)
	if next, _ := u.AutomaticSchedule(); next != first {
		t.Fatal("same-policy reload postponed the live deadline")
	}
	close(ready)
	uiTestAutomaticDone(t, started)
	if next, state := u.AutomaticSchedule(); next != first || state != "checking" {
		t.Fatalf("automatic operation did not expose its running state: %s / %s", next, state)
	}
	close(release)
	uiTestAutomaticWait(t, func() bool { _, state := u.AutomaticSchedule(); return state == "scheduled" })
	if next, _ := u.AutomaticSchedule(); next != first {
		t.Fatal("finishing an activation changed its periodic deadline")
	}
}

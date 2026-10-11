package dnsstats

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"testing"
	"time"
)

func TestUpstreamLatencySeparatesResultsAndExcludesObserverLock(t *testing.T) {
	m := New()
	ctx := WithManager(context.Background(), m)
	endpoint := "udp://192.0.2.53:53"
	for _, item := range []struct {
		result UpstreamResult
		age    time.Duration
	}{{UpstreamSuccess, 20 * time.Millisecond}, {UpstreamError, time.Second}, {UpstreamTimeout, 2 * time.Second}, {UpstreamRCodeError, 3 * time.Second}, {UpstreamCanceled, 500 * time.Millisecond}} {
		a := StartUpstream(ctx, endpoint)
		a.start = time.Now().Add(-item.age)
		a.Complete(item.result)
		a.Complete(item.result) // a repeated completion cannot alter any mean
	}
	s := m.Upstreams().Items[0]
	if s.Successes != 1 || s.Errors != 3 || s.Canceled != 1 || s.Attempts != 5 || s.Timeouts != 1 || s.RCodeErrors != 1 {
		t.Fatalf("result partitions changed: %+v", s)
	}
	if s.SuccessElapsedMSAvg < 20 || s.SuccessElapsedMSAvg > 100 || s.ErrorElapsedMSAvg < 2000 || s.ErrorElapsedMSAvg > 2100 || s.CanceledElapsedMSAvg < 500 || s.CanceledElapsedMSAvg > 600 {
		t.Fatalf("result latency mixed into another category: %+v", s)
	}
	want := (s.SuccessElapsedMSAvg + 3*s.ErrorElapsedMSAvg + s.CanceledElapsedMSAvg) / 5
	if math.Abs(s.ElapsedMSAvg-want) > 0.000001 {
		t.Fatalf("legacy mean is no longer the weighted complete mean: %f != %f", s.ElapsedMSAvg, want)
	}

	a := StartUpstream(ctx, "udp://192.0.2.54:53")
	m.mu.Lock()
	done := make(chan struct{})
	go func() { a.Complete(UpstreamSuccess); close(done) }()
	deadline := time.Now().Add(time.Second)
	for !a.finished.Load() && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if !a.finished.Load() {
		m.mu.Unlock()
		t.Fatal("completion did not capture its duration")
	}
	// The completion is now waiting only for the observation mutex.
	time.Sleep(100 * time.Millisecond)
	m.mu.Unlock()
	<-done
	for _, s := range m.Upstreams().Items {
		if s.Address == "udp://192.0.2.54:53" && s.SuccessElapsedMSAvg >= 100 {
			t.Fatalf("observation lock wait polluted DNS latency: %+v", s)
		}
	}
}

func TestUpstreamExitIdentityAndGlobalCapacity(t *testing.T) {
	m := New()
	ctx := WithManager(context.Background(), m)
	endpoint := "https://dns.example/dns-query"
	for _, exit := range []Outbound{
		{Key: "leaf-a", Name: "node-a", Type: "proxy"},
		{Key: "leaf-a", Name: "node-a", Type: "proxy"},
		{Key: "leaf-b", Name: "node-b", Type: "proxy"},
		{Key: "direct", Name: "DIRECT", Type: "direct"},
		{Type: "direct", Name: "DIRECT"}, // no proven identity must stay unknown
	} {
		StartUpstream(ctx, endpoint, exit).Complete(UpstreamSuccess)
	}
	rows := m.Upstreams().Items
	if len(rows) != 4 {
		t.Fatalf("distinct physical exits were merged: %+v", rows)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.ID] {
			t.Fatal("exit identities collided")
		}
		seen[row.ID] = true
		if row.Outbound == "node-a" && row.Attempts != 2 {
			t.Fatalf("same exit did not aggregate: %+v", row)
		}
		if row.OutboundType == "unknown" && row.Outbound != "" {
			t.Fatalf("unknown exit mislabeled: %+v", row)
		}
	}
	for i := 0; i < 300; i++ {
		StartUpstream(ctx, endpoint, Outbound{Key: fmt.Sprint(i), Name: "replaceable-node", Type: "proxy"}).Complete(UpstreamCanceled)
	}
	rows = m.Upstreams().Items
	if len(rows) != UpstreamLimit+1 {
		t.Fatalf("exit dimensions escaped the global cap: %d", len(rows))
	}
	var attempts uint64
	for _, row := range rows {
		attempts += row.Attempts
		if row.ID == "overflow" && (row.Outbound != "" || row.OutboundType != "unknown") {
			t.Fatalf("overflow pretends to be one exit: %+v", row)
		}
	}
	if attempts != 305 {
		t.Fatalf("overflow dropped attempts: %d", attempts)
	}
}

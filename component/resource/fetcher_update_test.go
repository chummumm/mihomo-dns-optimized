package resource

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/common/utils"
	P "github.com/metacubex/mihomo/constant/provider"
)

func TestFetcherHTTPCheckAppliesOnlyChangedValidatedContent(t *testing.T) {
	var body atomic.Value
	body.Store("old")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "rules.txt")
	vehicle := NewHTTPVehicle(server.URL, path, "", nil, time.Second, 1024)
	var applied string
	fetcher := NewFetcher("test", 0, vehicle, nil, func(data []byte) (string, error) {
		if bytes.Equal(data, []byte("invalid")) {
			return "", errors.New("invalid contents")
		}
		return string(data), nil
	}, func(value string) { applied = value })
	defer fetcher.Close()
	if _, _, err := fetcher.SideUpdate([]byte("old")); err != nil {
		t.Fatal(err)
	}
	if _, same, err := fetcher.UpdateContext(context.Background()); err != nil || !same {
		t.Fatalf("unchanged request = %v, %v", same, err)
	}
	body.Store("new")
	if _, same, err := fetcher.UpdateContext(context.Background()); err != nil || same {
		t.Fatalf("changed request = %v, %v", same, err)
	}
	if applied != "new" {
		t.Fatalf("new contents were not published: %q", applied)
	}
	body.Store("invalid")
	if _, _, err := fetcher.UpdateContext(context.Background()); err == nil {
		t.Fatal("invalid content was accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" || applied != "new" || requests.Load() != 3 {
		t.Fatalf("failed update changed state: disk=%q, live=%q, requests=%d, error=%v", data, applied, requests.Load(), err)
	}
	body.Store("later")
	rejected := errors.New("configuration changed")
	if _, _, err := fetcher.UpdateContextWithCommit(context.Background(), func(func() error) error { return rejected }); !errors.Is(err, rejected) {
		t.Fatalf("stale commit was not rejected: %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "new" || applied != "new" {
		t.Fatal("a stale configuration published its downloaded resource")
	}
}

type controlledVehicle struct {
	read  func(context.Context) ([]byte, error)
	write func([]byte) error
}

func (v *controlledVehicle) Read(ctx context.Context, _ utils.HashType) ([]byte, utils.HashType, error) {
	data, err := v.read(ctx)
	return data, utils.MakeHash(data), err
}
func (v *controlledVehicle) Write(data []byte) error { return v.write(data) }
func (*controlledVehicle) Path() string              { return "unused-test-path" }
func (*controlledVehicle) Url() string               { return "https://example.invalid/rules" }
func (*controlledVehicle) Proxy() string             { return "" }
func (*controlledVehicle) Type() P.VehicleType       { return P.HTTP }

func TestFetcherUpdateSerializesNetworkAndCancelsQueuedCall(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	vehicle := &controlledVehicle{
		read: func(context.Context) ([]byte, error) {
			if reads.Add(1) == 1 {
				close(started)
				<-release
			}
			return []byte("rules"), nil
		},
		write: func([]byte) error { return nil },
	}
	fetcher := NewFetcher("serialized", 0, vehicle, nil, func(data []byte) (string, error) { return string(data), nil }, nil)
	defer fetcher.Close()
	first := make(chan error, 1)
	go func() { _, _, err := fetcher.Update(); first <- err }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() { _, _, err := fetcher.UpdateContext(ctx); queued <- err }()
	cancel()
	select {
	case err := <-queued:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled call waited for another network request")
	}
	if reads.Load() != 1 {
		t.Fatalf("overlapping network read: %d", reads.Load())
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestFetcherCancelAfterReadDoesNotPublish(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var writes atomic.Int32
	vehicle := &controlledVehicle{
		read:  func(context.Context) ([]byte, error) { cancel(); return []byte("stale"), nil },
		write: func([]byte) error { writes.Add(1); return nil },
	}
	fetcher := NewFetcher("cancelled", 0, vehicle, nil, func(data []byte) (string, error) { return string(data), nil }, nil)
	defer fetcher.Close()
	if _, _, err := fetcher.UpdateContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("late cancellation = %v", err)
	}
	if writes.Load() != 0 || !fetcher.UpdatedAt().IsZero() {
		t.Fatal("cancelled response changed the installed resource")
	}
}

func TestHTTPVehicleRejectsOversizedResponseWithoutTruncating(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("four")) }))
	defer server.Close()
	vehicle := NewHTTPVehicle(server.URL, "unused", "", nil, time.Second, 3)
	data, hash, err := vehicle.Read(context.Background(), utils.HashType{})
	if err == nil || len(data) != 0 || hash.IsValid() {
		t.Fatalf("truncated response was usable: %q, %v, %v", data, hash, err)
	}
	for _, limit := range []int64{4, math.MaxInt64} {
		vehicle := NewHTTPVehicle(server.URL, "unused", "", nil, time.Second, limit)
		data, hash, err := vehicle.Read(context.Background(), utils.HashType{})
		if err != nil || string(data) != "four" || !hash.IsValid() {
			t.Fatalf("valid response under limit %d was rejected: %q %v", limit, data, err)
		}
	}
}

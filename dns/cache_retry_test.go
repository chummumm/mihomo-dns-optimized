package dns

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/tunnel"

	D "github.com/miekg/dns"
)

type cacheRetryGatedClient struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (c *cacheRetryGatedClient) Address() string  { return "test://gated-upstream" }
func (c *cacheRetryGatedClient) ResetConnection() {}
func (c *cacheRetryGatedClient) ExchangeContext(ctx context.Context, query *D.Msg) (*D.Msg, error) {
	if c.calls.Add(1) == 1 {
		close(c.started)
		select {
		case <-c.release:
			return nil, errors.New("first upstream exchange failed")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return new(D.Msg).SetReply(query), nil
}

func TestCacheControlForegroundRetryCannotOutliveInvalidation(t *testing.T) {
	for _, name := range []string{"Close", "Clear", "ModeRoundTrip"} {
		t.Run(name, func(t *testing.T) {
			client := &cacheRetryGatedClient{started: make(chan struct{}), release: make(chan struct{})}
			r := NewResolverFromClient(client)
			var release sync.Once
			t.Cleanup(func() {
				release.Do(func() { close(client.release) })
				r.Close()
			})
			query := new(D.Msg).SetQuestion("foreground-retry.example.", D.TypeA)
			// ExchangeContext will take the same generation before entering the
			// gated upstream. Save its flight key for a deterministic join below.
			ctx := r.cacheControl.Context(context.Background())
			flightKey := r.cacheControl.FlightKey(ctx, query.Question[0].String())
			finished := make(chan error, 1)
			go func() {
				_, err := r.ExchangeContext(ctx, query)
				finished <- err
			}()
			select {
			case <-client.started:
			case <-time.After(time.Second):
				t.Fatal("initial foreground exchange did not reach the upstream")
			}
			switch name {
			case "Close":
				r.Close()
			case "Clear":
				r.ClearCache()
			case "ModeRoundTrip":
				original := tunnel.Mode()
				other := tunnel.Direct
				if original == other {
					other = tunnel.Global
				}
				tunnel.SetMode(other)
				tunnel.SetMode(original)
			}
			release.Do(func() { close(client.release) })
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("synthetic upstream failure was lost")
				}
			case <-time.After(time.Second):
				t.Fatal("foreground exchange did not finish after release")
			}
			// The legacy foreground error path registers its retry in DoChan
			// before returning. Joining that key waits for any registered retry;
			// when none exists, this harmless barrier runs instead. No sleep or
			// probabilistic observation window is needed to detect a late dial.
			barrier := r.group.DoChan(flightKey, func() (*dnsExchangeResult, error) { return &dnsExchangeResult{Msg: new(D.Msg)}, nil })
			select {
			case <-barrier:
			case <-time.After(time.Second):
				t.Fatal("old generation retry did not unwind")
			}
			if got := client.calls.Load(); got != 1 {
				t.Fatalf("%s allowed an old foreground retry to contact the upstream: calls=%d", name, got)
			}
		})
	}
}

package dns

import (
	"context"
	"testing"
	"time"

	D "github.com/miekg/dns"
)

func responseExpiryTestResolver(t *testing.T, algorithm, mode string, firstAge time.Duration) (*Resolver, context.Context, *D.Msg, *selectionTestClient, <-chan time.Time, <-chan context.Context) {
	t.Helper()
	routingTestEnable(t)
	r := NewResolver(Config{
		RuleRouting:    true,
		Main:           []NameServer{{Addr: "192.0.2.53:53"}, {Addr: "192.0.2.54:53"}},
		CacheAlgorithm: algorithm, CacheOptions: &CacheOptions{ServeExpired: false},
		SpeedCheck: SpeedCheckConfig{Mode: []string{"tcp:443"}, Timeout: 2 * time.Second, Concurrency: 2, ResponseMode: mode},
	}).Resolver
	t.Cleanup(r.Close)
	received := make(chan time.Time, 1)
	started := make(chan context.Context, 4)
	first := &selectionTestClient{dnsClient: r.main[0], exchange: func(ctx context.Context, q *D.Msg) (*D.Msg, error) {
		ttl := uint32(30)
		if q.Id == 1 {
			ttl = 1
		}
		answer := cacheTestReply(q, ttl)
		if q.Id == 1 {
			at := time.Now().Add(-firstAge)
			recordDNSAnswerReceived(ctx, answer, at)
			received <- at
		}
		return answer, nil
	}}
	second := &selectionTestClient{dnsClient: r.main[1], exchange: func(ctx context.Context, q *D.Msg) (*D.Msg, error) {
		started <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	r.main = []dnsClient{first, second}
	query := routingTestQuery("response-expiry.example", 1)
	ctx := responseModePreparedContext(t, r, routingTestContext(newSelectionTestOutbound(), routingTestOrigin()), query)
	return r, ctx, query, first, received, started
}

func TestDNSResponseModeExhaustedAnswerDoesNotCreateReplayableFlight(t *testing.T) {
	for _, algorithm := range []string{"lru", "arc"} {
		for _, mode := range []string{ResponseModeFirstPing, ResponseModeFastestResponse} {
			t.Run(algorithm+"/"+mode, func(t *testing.T) {
				r, ctx, query, client, _, _ := responseExpiryTestResolver(t, algorithm, mode, 2*time.Second)
				answer := routingTestExchange(t, r, ctx, query)
				if answer.Answer[0].Header().Ttl != 0 || len(cacheBackgroundSlots) != 0 {
					t.Fatal("an exhausted answer did not complete normally without a continuation")
				}
				query2 := query.Copy()
				query2.Id++
				second := routingTestExchange(t, r, ctx, query2)
				if client.count(0) != 2 || second.Answer[0].Header().Ttl == 0 || second.Id != query2.Id {
					t.Fatalf("new transaction reused an exhausted answer: calls=%d answer=%v", client.count(0), second)
				}
			})
		}
	}
}

func TestDNSResponseModeExpiredSnapshotIsAtomicallyReplaced(t *testing.T) {
	for _, algorithm := range []string{"lru", "arc"} {
		for _, mode := range []string{ResponseModeFirstPing, ResponseModeFastestResponse} {
			t.Run(algorithm+"/"+mode, func(t *testing.T) {
				r, ctx, query, client, received, started := responseExpiryTestResolver(t, algorithm, mode, 500*time.Millisecond)
				first := routingTestExchange(t, r, ctx, query)
				at := responseModeReceive(t, received)
				oldCtx := responseModeReceive(t, started)
				if first.Answer[0].Header().Ttl != 1 || len(cacheBackgroundSlots) != 1 {
					t.Fatal("fixture did not publish a fresh answer with ongoing selection")
				}
				oldFinal := responseModeBarrier(r, ctx, query)
				// The second upstream stays active. Advance only to this answer's
				// absolute expiry, well before the original two-second work budget.
				timer := time.NewTimer(time.Until(at.Add(time.Second)) + 10*time.Millisecond)
				defer timer.Stop()
				responseModeReceive(t, timer.C)
				if len(cacheBackgroundSlots) != 1 {
					t.Fatal("old selection completed before the expiry test")
				}
				query2 := query.Copy()
				query2.Id++
				second := routingTestExchange(t, r, ctx, query2)
				if client.count(0) != 2 || second.Answer[0].Header().Ttl != 30 || second.Id != query2.Id {
					t.Fatalf("late transaction replayed an expired snapshot: calls=%d answer=%v", client.count(0), second)
				}
				// The new cache lease revokes the old background work. Its final
				// cleanup must leave the replacement singleflight and answer intact.
				responseModeReceive(t, oldCtx.Done())
				responseModeReceive(t, oldFinal)
				responseModeReceive(t, started)
				query3 := query2.Copy()
				query3.Id++
				third, err := r.exchangeWithoutCache(ctx, query3)
				if err != nil || client.count(0) != 2 || third.Id != query3.Id || third.Answer[0].Header().Ttl != 30 {
					t.Fatalf("old cleanup removed the fresh replacement: calls=%d answer=%v error=%v", client.count(0), third, err)
				}
				cached, _, hit := r.cache.GetWithExpire(r.cacheKey(ctx, query))
				if !hit || cached.Answer[0].Header().Ttl != 30 {
					t.Fatal("old completion overwrote the fresh replacement cache entry")
				}
				r.Close()
				if len(cacheBackgroundSlots) != 0 || len(dnsNativeWorkSlots) != 0 {
					t.Fatal("expired flight replacement leaked admission")
				}
			})
		}
	}
}

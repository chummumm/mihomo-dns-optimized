package dns

import (
	"context"
	"testing"
	"time"

	D "github.com/miekg/dns"
)

func TestCacheControlUncacheableRefreshInvalidatesOldAnswer(t *testing.T) {
	for _, algorithm := range []string{"lru", "arc"} {
		t.Run(algorithm, func(t *testing.T) {
			r := &Resolver{cache: (Config{CacheAlgorithm: algorithm, CacheMaxSize: 8}).newCache()}
			control := newCacheControl(r, &CacheOptions{ServeExpired: true, ServeExpiredTTL: 7 * 24 * time.Hour})
			defer control.Close()
			query, old := answerPolicyMessage(t, D.TypeAAAA, "www.example. 60 IN AAAA 2001:db8::1")
			key := dnsCacheKey(context.Background(), query.Question[0])
			r.cache.SetWithExpire(key, old, time.Now().Add(-time.Hour))
			if _, _, hit := getMsgFromCache(r.cache, key); !hit {
				t.Fatal("missing stale fixture")
			}
			if !control.Store(control.Context(context.Background()), key, query.Question[0], dualStackNoData(query, old)) {
				t.Fatal("current zero-TTL result was rejected")
			}
			if _, _, hit := getMsgFromCache(r.cache, key); hit {
				t.Fatal("a fresh zero-TTL answer left stale AAAA eligible for seven-day reuse")
			}
		})
	}
}

func TestCacheControlOldUncacheableAnswerCannotDeleteNewGeneration(t *testing.T) {
	r, control := cacheTestControl(t, nil)
	query := routingTestQuery("generation.example", 1)
	key := dnsCacheKey(context.Background(), query.Question[0])
	old := control.Context(context.Background())
	control.Clear()
	fresh := control.Context(context.Background())
	answer := cacheTestReply(query, 60)
	if !control.Store(fresh, key, query.Question[0], answer) {
		t.Fatal("fresh result not stored")
	}
	if control.Store(old, key, query.Question[0], dualStackNoData(query, answer)) {
		t.Fatal("retired query invalidated the new cache generation")
	}
	if _, _, hit := getMsgFromCache(r.cache, key); !hit {
		t.Fatal("old zero-TTL result erased fresh answer")
	}
}

func TestCacheControlTransientErrorDoesNotInvalidateStaleSuccess(t *testing.T) {
	r, control := cacheTestControl(t, nil)
	query := routingTestQuery("transient.example", 1)
	key := dnsCacheKey(context.Background(), query.Question[0])
	r.cache.SetWithExpire(key, cacheTestReply(query, 60), time.Now().Add(-time.Second))
	refused := new(D.Msg).SetRcode(query, D.RcodeRefused)
	control.Store(control.Context(context.Background()), key, query.Question[0], refused)
	if _, _, hit := getMsgFromCache(r.cache, key); !hit {
		t.Fatal("a transient refusal erased a usable stale answer")
	}
}

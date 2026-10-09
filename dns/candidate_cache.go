package dns

import (
	"context"
	"strconv"
	"sync"
	"time"

	C "github.com/metacubex/mihomo/constant"
	D "github.com/miekg/dns"
)

type dnsCandidateEntry struct {
	message                   *D.Msg
	reuseUntil, answerExpires time.Time
}
type dnsCandidateCache struct {
	mu         sync.RWMutex
	entries    map[string]dnsCandidateEntry
	generation uint64
	closed     bool
}

func newDNSCandidateCache() *dnsCandidateCache {
	return &dnsCandidateCache{entries: make(map[string]dnsCandidateEntry)}
}
func (c *dnsCandidateCache) get(key string) (*D.Msg, uint64) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	generation := c.generation
	closed := c.closed
	c.mu.RUnlock()
	now := time.Now()
	if closed || !ok || !now.Before(entry.reuseUntil) || !now.Before(entry.answerExpires) {
		return nil, generation
	}
	answer := entry.message.Copy()
	updateMsgTTL(answer, uint32(entry.answerExpires.Sub(now).Seconds()))
	return answer, generation
}
func (c *dnsCandidateCache) store(key string, generation uint64, query, answer *D.Msg) {
	if answer == nil || answer.Truncated || answer.Rcode != D.RcodeSuccess || !speedCheckResponseMatches(query, answer) {
		return
	}
	ttl := ^uint32(0)
	found := false
	for _, rrs := range [][]D.RR{answer.Answer, answer.Ns, answer.Extra} {
		for _, rr := range rrs {
			if rr.Header().Rrtype == D.TypeOPT {
				continue
			}
			found = true
			if rr.Header().Ttl < ttl {
				ttl = rr.Header().Ttl
			}
		}
	}
	if !found || ttl == 0 {
		return
	}
	now := time.Now()
	owned := answer.Copy()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || generation != c.generation {
		return
	}
	if len(c.entries) >= 1024 {
		for old := range c.entries {
			delete(c.entries, old)
			break
		}
	}
	c.entries[key] = dnsCandidateEntry{owned, now.Add(time.Second), now.Add(time.Duration(ttl) * time.Second)}
}
func (c *dnsCandidateCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.generation++
	c.entries = make(map[string]dnsCandidateEntry)
	c.mu.Unlock()
}
func (c *dnsCandidateCache) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.closed = true
	c.generation++
	c.entries = nil
	c.mu.Unlock()
}

type dnsCandidateClient struct {
	dnsClient
	cache *dnsCandidateCache
	index int
}

func (c *dnsCandidateClient) Unwrap() dnsClient { return c.dnsClient }
func (c *dnsCandidateClient) ExchangeContext(ctx context.Context, query *D.Msg) (*D.Msg, error) {
	route := queryRoute(ctx)
	if route == nil || route.err != nil || route.plan == nil {
		return c.dnsClient.ExchangeContext(ctx, query)
	}
	wire, err := query.Pack()
	if err != nil {
		return nil, err
	}
	key := makeDNSRouteKey(ctx, route, wire, true) + "|candidate:" + strconv.Itoa(c.index)
	cached, generation := c.cache.get(key)
	if cached != nil {
		cached.Id = query.Id
		return cached, nil
	}
	answer, err := c.dnsClient.ExchangeContext(ctx, query)
	if err == nil {
		c.cache.store(key, generation, query, answer)
	}
	return answer, err
}

// Reuse completed raw candidate answers briefly between primary A and
// auxiliary A/AAAA work. Live exchanges keep their own cancellation/trackers;
// there is no first-caller cancellation propagated to another query.
func (s *directSpeedChecker) shareCandidateClients(ctx context.Context, clients []dnsClient) []dnsClient {
	route := queryRoute(ctx)
	if s == nil || s.candidates == nil || route == nil || route.err != nil || route.plan == nil {
		return clients
	}
	if t := route.plan.Type(); t != C.Direct && t != C.Compatible {
		return clients
	}
	if automatic, explicit := dnsQueryCapabilities(&D.Msg{Question: []D.Question{{Qtype: D.TypeA, Qclass: D.ClassINET}}}, clients, nil); !automatic || explicit {
		return clients
	}
	wrapped := make([]dnsClient, len(clients))
	for i, c := range clients {
		wrapped[i] = &dnsCandidateClient{c, s.candidates, i}
	}
	return wrapped
}

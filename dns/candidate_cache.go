package dns

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	C "github.com/metacubex/mihomo/constant"
	D "github.com/miekg/dns"
)

type dnsCandidateEntry struct {
	message                               *D.Msg
	receivedAt, reuseUntil, answerExpires time.Time
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
	answer, generation, receivedAt := c.getRaw(key)
	return ageDNSAnswerAt(answer, receivedAt, time.Now()), generation
}

// The resolver applies its TTL policy once and ages from this original receipt
// time at completion. Returning a pre-aged raw copy here would age twice or
// allow rr-ttl-min to refill a partially consumed lifetime.
func (c *dnsCandidateCache) getRaw(key string) (*D.Msg, uint64, time.Time) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	generation := c.generation
	closed := c.closed
	c.mu.RUnlock()
	now := time.Now()
	if closed || !ok || !now.Before(entry.reuseUntil) || !now.Before(entry.answerExpires) {
		return nil, generation, time.Time{}
	}
	return entry.message.Copy(), generation, entry.receivedAt
}
func (c *dnsCandidateCache) store(key string, generation uint64, query, answer *D.Msg) {
	c.storeReceived(key, generation, query, answer, time.Now())
}

func (c *dnsCandidateCache) storeReceived(key string, generation uint64, query, answer *D.Msg, receivedAt time.Time) {
	if query == nil || answer == nil || answer.Truncated || answer.Rcode != D.RcodeSuccess || !speedCheckResponseMatches(query, answer) {
		return
	}
	// OPT options and signed messages can carry transaction-specific state.
	// Do not replay an upstream's EDNS cookies, ECS metadata or control fields
	// as another transaction's response. Live answers are returned unchanged;
	// this optional one-second cache simply declines these messages.
	if query.IsEdns0() != nil || answer.IsEdns0() != nil || speedCheckProtected(query) || speedCheckProtected(answer) {
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
	c.entries[key] = dnsCandidateEntry{
		message: owned, receivedAt: receivedAt,
		reuseUntil: receivedAt.Add(time.Second), answerExpires: receivedAt.Add(time.Duration(ttl) * time.Second),
	}
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
	cache              *dnsCandidateCache
	poolKey, serverKey string
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
	key := makeDNSRouteKey(ctx, route, wire, true) + "|candidate-pool:" + c.poolKey + "|server:" + c.serverKey
	cached, generation, receivedAt := c.cache.getRaw(key)
	if cached != nil {
		if hasDNSAnswerLifetimes(ctx) {
			recordDNSAnswerReceived(ctx, cached, receivedAt)
		} else {
			cached = ageDNSAnswerAt(cached, receivedAt, time.Now())
		}
		cached.Id = query.Id
		return cached, nil
	}
	answer, err := c.dnsClient.ExchangeContext(ctx, query)
	if err == nil {
		receivedAt = recordDNSAnswerReceived(ctx, answer, time.Now())
		c.cache.storeReceived(key, generation, query, answer, receivedAt)
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
	identities := make([]string, len(clients))
	var pool []byte
	for i, client := range clients {
		identity, ok := dnsCandidateServerIdentity(client)
		if !ok {
			// Unknown value clients may contain maps/slices and cannot safely
			// be interface map keys. An unproven identity never enables reuse.
			return clients
		}
		identities[i] = fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
		pool = binary.BigEndian.AppendUint32(pool, uint32(len(identity)))
		pool = append(pool, identity...)
	}
	poolKey := fmt.Sprintf("%x", sha256.Sum256(pool))
	wrapped := make([]dnsClient, len(clients))
	for i, c := range clients {
		wrapped[i] = &dnsCandidateClient{dnsClient: c, cache: s.candidates, poolKey: poolKey, serverKey: identities[i]}
	}
	return wrapped
}

// A stable configured transport instance plus every wrapper's semantics owns
// an answer. Address alone is insufficient: one transport can be reused with
// different ECS/disable-type wrappers. The selected ordered set is separately
// included in poolKey, so index zero in main and fallback cannot collide.
func dnsCandidateServerIdentity(client dnsClient) (string, bool) {
	switch c := client.(type) {
	case clientWithEdns0Subnet:
		inner, ok := dnsCandidateServerIdentity(c.dnsClient)
		return fmt.Sprintf("ecs:%s/override:%t|%s", c.ecsPrefix, c.ecsOverride, inner), ok
	case clientWithDisableTypes:
		inner, ok := dnsCandidateServerIdentity(c.dnsClient)
		types := make([]int, 0, len(c.disableTypes))
		for qtype := range c.disableTypes {
			types = append(types, int(qtype))
		}
		sort.Ints(types)
		var identity strings.Builder
		identity.WriteString("disable:")
		for _, qtype := range types {
			identity.WriteString(strconv.Itoa(qtype))
			identity.WriteByte(',')
		}
		identity.WriteByte('|')
		identity.WriteString(inner)
		return identity.String(), ok
	default:
		value := reflect.ValueOf(client)
		if !value.IsValid() || value.Kind() != reflect.Ptr || value.IsNil() {
			return "", false
		}
		return fmt.Sprintf("%T:%p", client, client), true
	}
}

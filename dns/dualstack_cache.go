package dns

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/netip"
	"sync"
	"time"

	D "github.com/miekg/dns"
)

// Selection evidence belongs to one exchange. Keep the positive primary
// answer separate from the transient family-preference SOA returned to a
// client; only a selected, successful exchange may commit either family.
type dnsDualStackAnswersKey struct{}
type dnsDualStackAnswers struct {
	r       *Resolver
	mu      sync.Mutex
	answers map[*D.Msg]*dnsDualStackSelection
}

type dnsDualStackSelection struct {
	positive          *D.Msg
	measurements      *dualStackMeasurements
	pool              string
	otherQuery        *D.Msg
	other             dualStackExchangeResult
	otherMeasurements *dualStackMeasurements
	otherCached       bool
}

func (r *Resolver) withDualStackAnswers(ctx context.Context) context.Context {
	if r.speedChecker == nil || !r.dualStack.Enabled {
		return ctx
	}
	return context.WithValue(ctx, dnsDualStackAnswersKey{}, &dnsDualStackAnswers{
		r: r, answers: make(map[*D.Msg]*dnsDualStackSelection),
	})
}

func recordDNSDualStackSelection(ctx context.Context, message *D.Msg, selection *dnsDualStackSelection) {
	state, _ := ctx.Value(dnsDualStackAnswersKey{}).(*dnsDualStackAnswers)
	if state == nil || message == nil || selection == nil {
		return
	}
	state.mu.Lock()
	state.answers[message] = selection
	state.mu.Unlock()
}

func dnsDualStackSelectionFor(ctx context.Context, message *D.Msg) *dnsDualStackSelection {
	state, _ := ctx.Value(dnsDualStackAnswersKey{}).(*dnsDualStackAnswers)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	selection := state.answers[message]
	state.mu.Unlock()
	return selection
}

func inheritDNSDualStackSelection(ctx context.Context, message, original *D.Msg) {
	recordDNSDualStackSelection(ctx, message, dnsDualStackSelectionFor(ctx, original))
}

func (r *Resolver) cacheableDualStackAnswer(ctx context.Context, query, message *D.Msg) *D.Msg {
	original := message
	if selection := dnsDualStackSelectionFor(ctx, message); selection != nil && selection.positive != nil {
		original = selection.positive
	}
	answer := r.answerPolicy.apply(query, original)
	answer = ageDNSAnswerAfterPolicy(ctx, original, answer)
	inheritDNSDualStackSelection(ctx, answer, message)
	inheritDNSLocalAnswer(ctx, answer, original)
	return answer
}

// A and AAAA use the same frozen outbound, source identity and selected pool,
// but must use distinct full wire-question keys. No group is selected again.
func otherFamilyCacheContext(ctx context.Context, query *D.Msg) (context.Context, bool) {
	route := queryRoute(ctx)
	if route == nil || route.plan == nil || route.err != nil || route.origin == nil {
		return ctx, false
	}
	wire, err := query.Pack()
	if err != nil {
		return ctx, false
	}
	other := &dnsQueryRoute{plan: route.plan, origin: route.origin, pool: route.pool,
		main: route.main, fallback: route.fallback}
	other.key = makeDNSRouteKey(ctx, other, wire, true)
	return context.WithValue(ctx, dnsQueryRouteKey{}, other), true
}

func dnsDualStackPool(clients []dnsClient) string {
	hash := sha256.New()
	for _, client := range clients {
		if candidate, ok := client.(*dnsCandidateClient); ok {
			client = candidate.dnsClient
		}
		identity, ok := dnsCandidateServerIdentity(client)
		if !ok {
			return ""
		}
		fmt.Fprintf(hash, "%d:%s", len(identity), identity)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

type dnsFamilySample struct {
	key, pool string
	rtt       time.Duration
	expires   time.Time
	identity  [32]byte
}

// TTL aging and transaction IDs do not change the address set being measured.
// Compare its complete alias path and ordered terminal addresses instead.
func dnsFamilyAnswerIdentity(question D.Question, message *D.Msg) [32]byte {
	chain, _ := speedCheckResponseChain(message, question)
	var identity []byte
	for _, link := range chain.links {
		for _, name := range []string{link.owner, link.target} {
			identity = binary.BigEndian.AppendUint32(identity, uint32(len(name)))
			identity = append(identity, name...)
		}
	}
	identity = binary.BigEndian.AppendUint32(identity, uint32(len(chain.owner)))
	identity = append(identity, chain.owner...)
	for _, candidate := range speedCheckCandidates(message, question) {
		identity = append(identity, candidate.ip.AsSlice()...)
	}
	return sha256.Sum256(identity)
}

// Evidence contains no messages or client contexts. It expires with its DNS
// answer and has a separate accounting budget, so a very large answer cache
// cannot retain an unbounded second copy of answers or origin metadata.
const dnsFamilySampleBudget = 8 * 1024 * 1024

type dnsFamilySampleCache struct {
	entries map[string]*list.Element
	order   list.List
	bytes   int
}

func dnsFamilySampleCost(sample dnsFamilySample) int { return 224 + len(sample.key) + len(sample.pool) }

func (c *dnsFamilySampleCache) remove(key string) {
	if entry := c.entries[key]; entry != nil {
		c.bytes -= dnsFamilySampleCost(entry.Value.(dnsFamilySample))
		c.order.Remove(entry)
		delete(c.entries, key)
	}
}

func (c *dnsFamilySampleCache) put(sample dnsFamilySample) {
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	c.remove(sample.key)
	for c.bytes+dnsFamilySampleCost(sample) > dnsFamilySampleBudget && c.order.Len() > 0 {
		c.remove(c.order.Back().Value.(dnsFamilySample).key)
	}
	c.entries[sample.key] = c.order.PushFront(sample)
	c.bytes += dnsFamilySampleCost(sample)
}

func (c *cacheControl) clearDualStackSamplesLocked() { c.dualStackSamples = nil }

// Called under the same generation/write lock as the corresponding DNS cache
// mutation. A newer unmeasured response invalidates the old measurement.
func (c *cacheControl) storeDualStackSampleLocked(ctx context.Context, key string, question D.Question, message *D.Msg) {
	if c.dualStackSamples != nil {
		c.dualStackSamples.remove(key)
	}
	if !c.r.dualStack.Enabled || c.r.cache == nil {
		return
	}
	selection := dnsDualStackSelectionFor(ctx, message)
	if selection == nil || selection.measurements == nil || selection.pool == "" {
		return
	}
	query := &D.Msg{MsgHdr: D.MsgHdr{Id: message.Id, Opcode: message.Opcode}, Question: []D.Question{question}}
	rtt, measured := selection.measurements.fastest(query, message)
	_, expires, cached := c.r.cache.GetWithExpire(key)
	if !measured || !cached || !expires.After(c.now()) {
		return
	}
	if c.dualStackSamples == nil {
		c.dualStackSamples = &dnsFamilySampleCache{}
	}
	c.dualStackSamples.put(dnsFamilySample{key: key, pool: selection.pool, rtt: rtt, expires: expires,
		identity: dnsFamilyAnswerIdentity(question, message)})
}

func (c *cacheControl) dualStackSampleLocked(key string, now time.Time) (*D.Msg, dnsFamilySample, bool) {
	if c.dualStackSamples == nil || c.r.cache == nil {
		return nil, dnsFamilySample{}, false
	}
	entry := c.dualStackSamples.entries[key]
	if entry == nil {
		return nil, dnsFamilySample{}, false
	}
	sample := entry.Value.(dnsFamilySample)
	message, expires, cached := c.r.cache.GetWithExpire(key)
	if !cached || !expires.Equal(sample.expires) || !now.Before(expires) {
		c.dualStackSamples.remove(key)
		return nil, dnsFamilySample{}, false
	}
	c.dualStackSamples.order.MoveToFront(entry)
	return message, sample, true
}

func cachedDNSDualStackFamily(ctx context.Context, clients []dnsClient, query *D.Msg) (dualStackExchangeResult, *dualStackMeasurements, bool) {
	state, _ := ctx.Value(dnsDualStackAnswersKey{}).(*dnsDualStackAnswers)
	if state == nil || state.r.cacheControl == nil {
		return dualStackExchangeResult{}, nil, false
	}
	otherCtx, ok := otherFamilyCacheContext(ctx, query)
	if !ok {
		return dualStackExchangeResult{}, nil, false
	}
	c := state.r.cacheControl
	key, pool := state.r.cacheKey(otherCtx, query), dnsDualStackPool(clients)
	c.mu.Lock()
	c.syncPolicyLocked()
	if !c.validLocked(ctx) {
		c.mu.Unlock()
		return dualStackExchangeResult{}, nil, false
	}
	message, sample, hit := c.dualStackSampleLocked(key, c.now())
	if !hit || sample.pool != pool {
		c.mu.Unlock()
		return dualStackExchangeResult{}, nil, false
	}
	answer := message.Copy()
	c.mu.Unlock()
	answer.Id = query.Id
	remaining := time.Until(sample.expires)
	if remaining <= 0 {
		return dualStackExchangeResult{}, nil, false
	}
	updateMsgTTL(answer, uint32(remaining.Seconds()))
	if !speedCheckAcceptResponse(ctx, answer) {
		return dualStackExchangeResult{}, nil, false
	}
	measurements := &dualStackMeasurements{rtts: make(map[netip.Addr]time.Duration)}
	for _, candidate := range speedCheckCandidates(answer, query.Question[0]) {
		measurements.rtts[candidate.ip] = sample.rtt
	}
	return dualStackExchangeResult{message: answer, cache: true}, measurements, true
}

// A fresh primary cache hit can reuse valid evidence from the other family.
// This never performs network I/O, changes routing, or stores the transient SOA.
func (r *Resolver) preferCachedDualStack(ctx context.Context, query, answer *D.Msg, key string, expires time.Time) *D.Msg {
	if r.cacheControl == nil || !r.dualStack.Enabled || !r.ipv6 || r.speedChecker == nil ||
		!speedCheckQuestion(query) || !answerPolicyCanRewrite(query, answer) ||
		len(speedCheckCandidates(answer, query.Question[0])) == 0 ||
		r.answerPolicy.forcesQtype(D.TypeA) || r.answerPolicy.forcesQtype(D.TypeAAAA) ||
		(query.Question[0].Qtype == D.TypeA && !r.dualStack.AllowForceAAAA) {
		return answer
	}
	other := query.Copy()
	if other.Question[0].Qtype == D.TypeA {
		other.Question[0].Qtype = D.TypeAAAA
	} else {
		other.Question[0].Qtype = D.TypeA
	}
	otherCtx, ok := otherFamilyCacheContext(ctx, other)
	if !ok {
		return answer
	}
	c := r.cacheControl
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncPolicyLocked()
	if !c.validLocked(ctx) {
		return answer
	}
	now := c.now()
	_, primary, primaryOK := c.dualStackSampleLocked(key, now)
	_, auxiliary, otherOK := c.dualStackSampleLocked(r.cacheKey(otherCtx, other), now)
	if primaryOK && otherOK && primary.expires.Equal(expires) &&
		primary.identity == dnsFamilyAnswerIdentity(query.Question[0], answer) &&
		primary.pool == auxiliary.pool && primary.rtt-auxiliary.rtt >= r.dualStack.Threshold {
		return dualStackNoData(query, answer)
	}
	return answer
}

// Auxiliary answers use the actual positive RRset and its original lifetime.
// Never overwrite a fresh foreground result or an in-flight continuation for
// that family. The generation lock prevents clear/reload races.
func (r *Resolver) storeDualStackAuxiliary(ctx context.Context, original *D.Msg) {
	selection := dnsDualStackSelectionFor(ctx, original)
	if selection == nil || selection.otherCached || selection.otherQuery == nil || selection.other.err != nil ||
		!selection.other.cache || selection.otherMeasurements == nil || r.cacheControl == nil {
		return
	}
	query, message := selection.otherQuery, selection.other.message
	if !speedCheckQuestion(query) || !answerPolicyCanRewrite(query, message) || len(speedCheckCandidates(message, query.Question[0])) == 0 {
		return
	}
	otherCtx, ok := otherFamilyCacheContext(ctx, query)
	if !ok {
		return
	}
	answer := r.answerPolicy.apply(query, message)
	answer = ageDNSAnswerAfterPolicy(ctx, message, answer)
	recordDNSDualStackSelection(ctx, answer, &dnsDualStackSelection{
		positive: message, measurements: selection.otherMeasurements, pool: selection.pool,
	})
	key := r.cacheKey(otherCtx, query)
	var staged stagedDNSWrite
	putMsgToCacheWithExpiry(&staged, key, query.Question[0], answer, dnsAnswerExpires(ctx, answer))
	if staged.message == nil {
		return
	}
	c := r.cacheControl
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncPolicyLocked()
	if !c.validLocked(ctx) || c.selections[key] != nil || dnsQueryClosed(ctx) {
		return
	}
	if lease, _ := ctx.Value(dnsResponseContinuationKey{}).(*dnsResponseContinuation); lease != nil && c.selections[lease.key] != lease {
		return
	}
	if _, expires, hit := r.cache.GetWithExpire(key); hit && expires.After(c.now()) {
		return
	}
	staged.apply(r.cache)
	c.storeDualStackSampleLocked(ctx, key, query.Question[0], answer)
	// Updating an already demanded family should track its new lifetime, but
	// an auxiliary exchange is not foreground demand for that family's key.
	c.storedPrefetchLocked(context.WithValue(ctx, cacheBackgroundKey{}, true), key)
}

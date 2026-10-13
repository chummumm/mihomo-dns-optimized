package dns

import (
	"container/heap"
	"container/list"
	"context"
	"hash/fnv"
	"strings"
	"time"

	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"

	D "github.com/miekg/dns"
)

const (
	cachePrefetchInitialHeat = 3
	cacheRefreshMinPeriod    = time.Second
	cachePrefetchRetryBase   = 5 * time.Second
	cachePrefetchRetryMax    = 5 * time.Minute
	cachePrefetchTickBatch   = 256

	// Account for the source snapshot, candidate, map/list/heap indexes and
	// owned variable-size data. Large answer caches do not imply equally many
	// retained request contexts: candidates own no parent context or timer.
	// A typical small query fits roughly 12,000--16,000 candidates in 16 MiB;
	// this is an accounting budget, not a promise about Go's total heap/RSS.
	cachePrefetchBudget        = 16 << 20
	cachePrefetchEntryLimit    = 16 << 10
	cachePrefetchEntryOverhead = 1024
)

type cachePrefetchDemandKey struct{}

// A miss retains this only in its bounded foreground exchange. A successful
// cacheable write claims it once, including when response selection publishes
// an early reply and subsequently replaces that same answer.
type cachePrefetchDemand struct {
	control *cacheControl
	key     string
	request cacheRefreshRequest
	claimed bool // protected by cacheControl.mu
}

type cacheRefreshRequest struct {
	origin    *C.Metadata
	fixed     C.ProxyAdapter
	wire      []byte
	bytes     int
	inbound   bool
	bootstrap bool
}

type cacheHotEntry struct {
	key        string
	request    cacheRefreshRequest
	expires    time.Time
	next       time.Time
	heat       uint8
	failures   uint8
	running    bool
	demand     uint64
	revision   uint64
	order      uint64
	bytes      int
	queueIndex int
	element    *list.Element
}

// Ordering by deadline prevents a map iteration or fresh demand from
// repeatedly choosing newer work ahead of an already overdue candidate.
type cachePrefetchQueue []*cacheHotEntry

func (q cachePrefetchQueue) Len() int { return len(q) }
func (q cachePrefetchQueue) Less(i, j int) bool {
	if q[i].next.Equal(q[j].next) {
		return q[i].order < q[j].order
	}
	return q[i].next.Before(q[j].next)
}
func (q cachePrefetchQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].queueIndex, q[j].queueIndex = i, j
}
func (q *cachePrefetchQueue) Push(value any) {
	entry := value.(*cacheHotEntry)
	entry.queueIndex = len(*q)
	*q = append(*q, entry)
}
func (q *cachePrefetchQueue) Pop() any {
	last := len(*q) - 1
	entry := (*q)[last]
	(*q)[last] = nil
	*q = (*q)[:last]
	entry.queueIndex = -1
	return entry
}

// The logical route alone is insufficient: a nameserver policy or an explicit
// exit can bypass it. Only a resolved DIRECT/Compatible leaf and an entirely
// automatic effective pool are eligible. Unknown/explicit pools remain on the
// ordinary on-demand path even when their display name happens to be DIRECT.
func (r *Resolver) directDNSQueryEligible(ctx context.Context, query *D.Msg) bool {
	if query == nil || len(query.Question) != 1 {
		return false
	}
	route := queryRoute(ctx)
	if route == nil || route.err != nil || route.plan == nil {
		return false
	}
	if kind := route.plan.Type(); kind != C.Direct && kind != C.Compatible {
		return false
	}
	main, fallback := r.dnsQueryServers(ctx)
	if matched := r.matchPolicy(query); len(matched) != 0 {
		main, fallback = matched, nil
	} else if isIPRequest(query.Question[0]) && len(fallback) != 0 && r.shouldOnlyQueryFallback(query) {
		main, fallback = fallback, nil
	}
	automatic, explicit := dnsQueryCapabilities(query, main, fallback)
	return automatic && !explicit
}

// Observe is called after current routing and before cache lookup. A miss does
// not create a global pending candidate: only its eventual cacheable success
// activates one. Internal/bootstrap queries and background refreshes never add
// heat, and a source's first successful real query already has finite credit.
func (c *cacheControl) Observe(ctx context.Context, query *D.Msg, key string) context.Context {
	if !c.options.Prefetch || cacheBackground(ctx) || icontext.DNSBootstrap(ctx) || !icontext.DNSRoutingInbound(ctx) || !c.r.directDNSQueryEligible(ctx, query) {
		return ctx
	}
	request, ok := newCachePrefetchRequest(ctx, query)
	if !ok {
		return ctx
	}
	demand := &cachePrefetchDemand{control: c, key: key, request: request}
	ctx = context.WithValue(ctx, cachePrefetchDemandKey{}, demand)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncPolicyLocked()
	if !c.validLocked(ctx) {
		return ctx
	}
	if expires, ok := c.cachedPrefetchAnswerLocked(key, c.now()); ok {
		c.activatePrefetchLocked(key, demand, expires)
	}
	return ctx
}

func newCachePrefetchRequest(ctx context.Context, query *D.Msg) (cacheRefreshRequest, bool) {
	if query.Response || query.Opcode != D.OpcodeQuery {
		return cacheRefreshRequest{}, false
	}
	for _, rr := range query.Extra {
		if rr.Header().Rrtype == D.TypeTSIG {
			return cacheRefreshRequest{}, false
		}
		if signature, ok := rr.(*D.SIG); ok && signature.TypeCovered == 0 {
			return cacheRefreshRequest{}, false
		}
	}
	request, ok := newCacheRefreshRequest(ctx, query)
	if !ok || request.origin == nil || !request.origin.SourceValid() {
		return cacheRefreshRequest{}, false
	}
	// Transaction IDs are local, and signed queries were excluded above.
	request.wire[0], request.wire[1] = 0, 0
	return request, true
}

func newCacheRefreshRequest(ctx context.Context, query *D.Msg) (cacheRefreshRequest, bool) {
	origin := icontext.DNSRoutingMetadata(ctx)
	if route := queryRoute(ctx); route != nil && route.plan != nil {
		origin = route.plan.OriginMetadata()
	}
	wire, err := query.Pack()
	if err != nil {
		return cacheRefreshRequest{}, false
	}
	var fields []*string
	var lists [][]string
	if origin != nil {
		origin.RawSrcAddr, origin.RawDstAddr = nil, nil
		fields = []*string{&origin.SrcIPASN, &origin.DstIPASN, &origin.InName, &origin.InUser,
			&origin.RematchName, &origin.Host, &origin.Process, &origin.ProcessPath,
			&origin.SpecialProxy, &origin.SpecialRules, &origin.RemoteDst, &origin.SniffHost}
		lists = [][]string{origin.SrcGeoIP, origin.DstGeoIP}
	}
	bytes := cachePrefetchEntryOverhead + cap(wire)
	for _, field := range fields {
		bytes += len(*field)
	}
	for _, values := range lists {
		for _, value := range values {
			bytes += 32 + len(value)
		}
	}
	if bytes > cachePrefetchEntryLimit {
		return cacheRefreshRequest{}, false
	}
	// Own string backing storage rather than retaining a larger inbound buffer
	// through a small substring. Raw address objects and request contexts are
	// unnecessary after the process/source snapshot has been captured.
	for _, field := range fields {
		*field = strings.Clone(*field)
	}
	cloneStrings := func(values []string) []string {
		if values == nil {
			return nil
		}
		owned := make([]string, len(values))
		for i, value := range values {
			owned[i] = strings.Clone(value)
		}
		return owned
	}
	if origin != nil {
		origin.SrcGeoIP, origin.DstGeoIP = cloneStrings(origin.SrcGeoIP), cloneStrings(origin.DstGeoIP)
	}
	return cacheRefreshRequest{
		origin: origin, fixed: icontext.DNSFixedOutbound(ctx), wire: wire, bytes: bytes,
		inbound: icontext.DNSRoutingInbound(ctx), bootstrap: icontext.DNSBootstrap(ctx),
	}, true
}

func (r cacheRefreshRequest) context(control *cacheControl) context.Context {
	ctx := context.WithValue(context.Background(), cacheGenerationKey{}, cacheGeneration{control, control.generation})
	ctx = icontext.WithDNSRoutingMetadata(ctx, r.origin)
	ctx = icontext.WithDNSProcessSnapshot(ctx)
	if r.inbound {
		ctx = icontext.WithDNSRoutingInbound(ctx)
	}
	if r.bootstrap {
		ctx = icontext.WithDNSBootstrap(ctx)
	}
	if r.fixed != nil {
		ctx = icontext.WithDNSFixedOutbound(ctx, r.fixed)
	}
	return ctx
}

func (c *cacheControl) cachedPrefetchAnswerLocked(key string, now time.Time) (time.Time, bool) {
	if c.r.cache == nil {
		return time.Time{}, false
	}
	message, expires, exists := c.r.cache.GetWithExpire(key)
	if !exists || message == nil || message.Rcode != D.RcodeSuccess || message.Truncated || expires.IsZero() {
		return time.Time{}, false
	}
	if !expires.After(now) {
		if ok, _ := c.ServeStale(expires, now); !ok {
			return time.Time{}, false
		}
	}
	return expires, true
}

func (c *cacheControl) activatePrefetchLocked(key string, demand *cachePrefetchDemand, expires time.Time) {
	if demand == nil || demand.control != c || demand.key != key || demand.claimed {
		return
	}
	demand.claimed = true
	bytes := demand.request.bytes + len(key)
	if bytes > cachePrefetchEntryLimit || bytes > c.hotBudget {
		return
	}
	entry := c.hot[key]
	extra := bytes
	if entry != nil {
		extra -= entry.bytes
	}
	for c.hotBytes+extra > c.hotBudget {
		oldest := c.lru.Back()
		if oldest != nil && oldest.Value.(*cacheHotEntry) == entry {
			oldest = oldest.Prev()
		}
		if oldest == nil {
			return
		}
		c.removeHotLocked(oldest.Value.(*cacheHotEntry))
	}
	if entry == nil {
		entry = &cacheHotEntry{key: strings.Clone(key), queueIndex: -1}
		entry.element = c.lru.PushFront(entry)
		c.hot[key] = entry
	} else {
		c.lru.MoveToFront(entry.element)
	}
	c.hotBytes += extra
	entry.request, entry.bytes = demand.request, bytes
	entry.heat = cachePrefetchInitialHeat
	entry.demand++
	if entry.next.IsZero() {
		entry.expires = expires
		c.schedulePrefetchLocked(entry, c.now())
	}
	// A cache hit replenishes credit without postponing an existing deadline.
	c.startLocked()
}

func (c *cacheControl) storedPrefetchLocked(ctx context.Context, key string) {
	if !c.options.Prefetch {
		return
	}
	expires, exists := c.cachedPrefetchAnswerLocked(key, c.now())
	if !exists || !expires.After(c.now()) {
		if entry := c.hot[key]; entry != nil {
			c.removeHotLocked(entry)
			c.stopIdleLocked()
		}
		return
	}
	if !cacheBackground(ctx) {
		demand, _ := ctx.Value(cachePrefetchDemandKey{}).(*cachePrefetchDemand)
		c.activatePrefetchLocked(key, demand, expires)
	}
	if entry := c.hot[key]; entry != nil {
		entry.expires = expires
		entry.revision++
		entry.failures = 0
		c.schedulePrefetchLocked(entry, c.now())
	}
}

func (c *cacheControl) schedulePrefetchLocked(entry *cacheHotEntry, now time.Time) {
	// Fresh entries refresh halfway through their remaining TTL. The expired
	// interval only applies when real demand activates an already stale answer;
	// it neither changes fresh deadlines nor postpones overdue work or retries.
	delay := entry.expires.Sub(now) / 2
	if !entry.expires.After(now) {
		delay = entry.expires.Add(c.expiredPrefetchPeriod()).Sub(now)
	}
	if delay < cacheRefreshMinPeriod {
		delay = cacheRefreshMinPeriod
	}
	entry.next = now.Add(delay)
	c.enqueuePrefetchLocked(entry)
}

func (c *cacheControl) expiredPrefetchPeriod() time.Duration {
	period := c.options.ServeExpiredPrefetchTime
	if period == 0 {
		period = c.options.ServeExpiredTTL / 2
		if period == 0 || period > 8*time.Hour {
			period = 8 * time.Hour
		}
	}
	if period < cacheRefreshMinPeriod {
		period = cacheRefreshMinPeriod
	}
	return period
}

func (c *cacheControl) enqueuePrefetchLocked(entry *cacheHotEntry) {
	if entry.running {
		return
	}
	if entry.queueIndex >= 0 {
		heap.Fix(&c.ready, entry.queueIndex)
		return
	}
	c.sequence++
	entry.order = c.sequence
	heap.Push(&c.ready, entry)
}

func (c *cacheControl) launchDuePrefetchLocked(now time.Time) {
	var blocked []*cacheHotEntry
	defer func() {
		for _, entry := range blocked {
			if c.hot[entry.key] == entry {
				heap.Push(&c.ready, entry)
			}
		}
	}()
	for checked := 0; checked < cachePrefetchTickBatch && len(c.ready) != 0; checked++ {
		entry := c.ready[0]
		if entry.next.After(now) {
			break
		}
		if _, exists := c.inflight[entry.key]; exists {
			// An on-demand refresh may already own this key. It must not block
			// other due entries at the head of the deadline queue.
			blocked = append(blocked, heap.Pop(&c.ready).(*cacheHotEntry))
			continue
		}
		if _, valid := c.cachedPrefetchAnswerLocked(entry.key, now); !valid || entry.heat <= 1 {
			c.removeHotLocked(entry)
			continue
		}
		query := new(D.Msg)
		if err := query.Unpack(entry.request.wire); err != nil {
			c.removeHotLocked(entry)
			continue
		}
		if !c.launchRefreshLocked(entry.request.context(c), query, entry.key, entry) {
			// No slot: retain the original deadline/order and all heat. The
			// next tick can retry without allocating a waiting goroutine.
			break
		}
	}
}

func (c *cacheControl) beginPrefetchLocked(entry *cacheHotEntry) {
	if entry.queueIndex >= 0 {
		heap.Remove(&c.ready, entry.queueIndex)
	}
	entry.running = true
	entry.heat--
}

func (c *cacheControl) finishPrefetchLocked(job cacheRefreshJob) {
	entry := job.prefetch
	if entry == nil || c.closed || c.generation != job.generation || c.hot[job.key] != entry {
		return
	}
	entry.running = false
	if entry.heat <= 1 {
		// One successful client query grants at most two admitted attempts,
		// including failed attempts. Background work never renews that lease.
		c.removeHotLocked(entry)
		return
	}
	if entry.revision == job.revision {
		// No cacheable successful write. Keep the old expiration untouched;
		// retry briefly, using only the remaining finite credit.
		if entry.failures < 8 {
			entry.failures++
		}
		shift := entry.failures - 1
		if shift > 6 {
			shift = 6
		}
		delay := cachePrefetchRetryBase << shift
		if delay > cachePrefetchRetryMax {
			delay = cachePrefetchRetryMax
		}
		hash := fnv.New32a()
		_, _ = hash.Write([]byte(entry.key))
		delay += time.Duration(hash.Sum32()%1000) * time.Millisecond
		entry.next = c.now().Add(delay)
	}
	c.enqueuePrefetchLocked(entry)
}

func (c *cacheControl) removeHotLocked(entry *cacheHotEntry) {
	if c.hot[entry.key] != entry {
		return
	}
	if entry.queueIndex >= 0 {
		heap.Remove(&c.ready, entry.queueIndex)
	}
	delete(c.hot, entry.key)
	c.lru.Remove(entry.element)
	c.hotBytes -= entry.bytes
	if len(c.hot) == 0 {
		c.hot = make(map[string]*cacheHotEntry)
		c.ready = nil
	}
}

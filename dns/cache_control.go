package dns

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/contextutils"
	"github.com/metacubex/mihomo/component/resolver"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/tunnel"

	D "github.com/miekg/dns"
)

// CacheOptions configures reuse of expired answers and proactive refreshes.
// A nil option preserves upstream behavior: expired answers have TTL 1, no
// maximum stale age, and there is no proactive prefetch.
type CacheOptions struct {
	ServeExpired         bool
	ServeExpiredTTL      time.Duration
	ServeExpiredReplyTTL uint32
	Prefetch             bool
	// Expired-only scheduled interval; zero derives half the stale retention,
	// capped at eight hours. It neither enables Prefetch nor delays demand.
	ServeExpiredPrefetchTime time.Duration
}

const DefaultServeExpiredPrefetchTime = 8 * time.Hour

// Shared by all resolver instances, including their separate upstream pools.
// Admission is nonblocking: overload never creates an unbounded goroutine queue.
var cacheBackgroundSlots = make(chan struct{}, 16)

type cacheGenerationKey struct{}
type cacheBackgroundKey struct{}

type cacheGeneration struct {
	control *cacheControl
	number  uint64
}

func cacheBackground(ctx context.Context) bool {
	value, _ := ctx.Value(cacheBackgroundKey{}).(bool)
	return value
}

type cacheRefreshJob struct {
	key        string
	ctx        context.Context
	query      *D.Msg
	generation uint64
	lifetime   context.Context
	prefetch   *cacheHotEntry
	revision   uint64
	demand     uint64
}

type cacheScheduler struct {
	stop chan struct{}
	done chan struct{}
}

type cacheControl struct {
	r       *Resolver
	options CacheOptions

	mu               sync.Mutex
	generation       uint64
	epoch            uint64
	lifetime         context.Context
	cancel           context.CancelFunc
	closed           bool
	scheduler        *cacheScheduler
	wg               sync.WaitGroup
	hot              map[string]*cacheHotEntry
	lru              list.List
	ready            cachePrefetchQueue
	hotBytes         int
	hotBudget        int
	sequence         uint64
	inflight         map[string]uint64
	pending          map[string]*cachePendingRefresh
	pendingOrder     list.List
	pendingBytes     int
	selections       map[string]*dnsResponseContinuation
	dualStackSamples *dnsFamilySampleCache

	// One clock and one ticker per active control, never a timer per domain.
	now       func() time.Time
	tickEvery time.Duration
	fetch     func(context.Context, *D.Msg) (*D.Msg, error)
}

func newCacheControl(r *Resolver, options *CacheOptions) *cacheControl {
	value := CacheOptions{ServeExpired: true, ServeExpiredReplyTTL: 1}
	if options != nil {
		value = *options
	}
	if value.ServeExpiredPrefetchTime < 0 {
		// Parsed configurations reject negative intervals. Programmatic callers
		// must not accidentally start an invalid proactive refresh policy.
		value.Prefetch = false
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &cacheControl{
		r: r, options: value, generation: 1, epoch: tunnel.DNSRoutingEpoch(),
		lifetime: ctx, cancel: cancel,
		hot: make(map[string]*cacheHotEntry), inflight: make(map[string]uint64),
		hotBudget: cachePrefetchBudget,
		now:       time.Now, tickEvery: time.Second, fetch: r.exchangeWithoutCache,
	}
}

// Context is called at the start of each foreground logical query. Store and
// FlightKey retain this generation even if Clear or a mode change happens later.
func (c *cacheControl) Context(ctx context.Context) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncPolicyLocked()
	return context.WithValue(ctx, cacheGenerationKey{}, cacheGeneration{c, c.generation})
}

func (c *cacheControl) validLocked(ctx context.Context) bool {
	stamp, ok := ctx.Value(cacheGenerationKey{}).(cacheGeneration)
	return !c.closed && ok && stamp.control == c && stamp.number == c.generation
}

// FlightKey isolates an old, still-unwinding singleflight from queries after a
// clear, reload or mode change. It is intentionally separate from the cache key.
func (c *cacheControl) FlightKey(ctx context.Context, key string) string {
	key = dnsQueryFlightKey(ctx, key)
	stamp, ok := ctx.Value(cacheGenerationKey{}).(cacheGeneration)
	if !ok || stamp.control != c {
		return key
	}
	// A foreground client must not join a prefetch which can be canceled when
	// its scheduler closes or policy changes. Each class still deduplicates.
	return fmt.Sprintf("cache-generation:%d/background:%t|%s", stamp.number, cacheBackground(ctx), key)
}

// ServeStale compares against the original cached expiration, never an access
// timestamp. A zero maximum age means the upstream unlimited-stale behavior.
func (c *cacheControl) ServeStale(expire, now time.Time) (bool, uint32) {
	if !c.options.ServeExpired || expire.After(now) {
		return false, 0
	}
	if c.options.ServeExpiredTTL > 0 && now.Sub(expire) > c.options.ServeExpiredTTL {
		return false, 0
	}
	return true, c.options.ServeExpiredReplyTTL
}

// Refresh admits one bounded request-triggered refresh, independently of the
// Prefetch option. The old request's rule plan is never reused by the worker.
func (c *cacheControl) Refresh(ctx context.Context, query *D.Msg, key string) {
	if query == nil || len(query.Question) != 1 || cacheBackground(ctx) {
		return
	}
	ctx, _ = cacheRefreshContext(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncPolicyLocked()
	if !c.validLocked(ctx) {
		return
	}
	if _, exists := c.inflight[key]; exists {
		return
	}
	now := c.now()
	if item := c.pending[key]; item != nil {
		if now.Before(item.expires) {
			c.launchPendingRefreshLocked(now)
			c.stopIdleLocked()
			return
		}
		// A newly arrived request must not disappear into an expired item
		// merely because its controller's next periodic tick has not run yet.
		c.removePendingRefreshLocked(item)
	}
	c.launchPendingRefreshLocked(now)
	if !c.launchLocked(ctx, query, key) {
		c.queueRefreshLocked(ctx, query, key)
	}
}

// Freeze both known and unknown PROCESS results. A scheduled refresh must not
// identify a new process which happened to reuse the old client's source port.
func cacheRefreshContext(ctx context.Context) (context.Context, bool) {
	origin := icontext.DNSRoutingMetadata(ctx)
	if route := queryRoute(ctx); route != nil && route.plan != nil {
		origin = route.plan.OriginMetadata()
	}
	if origin != nil {
		ctx = icontext.WithDNSRoutingMetadata(ctx, origin)
	}
	ctx = icontext.WithDNSProcessSnapshot(contextutils.WithoutCancel(ctx))
	return ctx, origin != nil && origin.SourceValid()
}

// Store is the sole generation-aware write hook. Clear holds the same mutex,
// so a response from an older query cannot refill the cache after it was cleared.
func (c *cacheControl) Store(ctx context.Context, key string, question D.Question, message *D.Msg) bool {
	if message == nil {
		return false
	}
	var staged stagedDNSWrite
	expires := dnsAnswerExpires(ctx, message)
	if c.r.ruleRouting {
		putMsgToCacheWithExpiry(&staged, key, question, message, expires)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncPolicyLocked()
	if !c.validLocked(ctx) || message == nil || c.r.cache == nil {
		return false
	}
	if !c.acceptResponseWriteLocked(ctx, key) {
		return false
	}
	if c.r.ruleRouting {
		staged.apply(c.r.cache)
	} else {
		putMsgToCacheWithExpiry(c.r.cache, key, question, message, expires)
	}
	c.storeDualStackSampleLocked(ctx, key, question, message)
	c.storedPrefetchLocked(ctx, key)
	return true
}

func (c *cacheControl) startLocked() {
	if c.scheduler != nil || c.closed || len(c.hot)+len(c.inflight)+len(c.pending) == 0 {
		return
	}
	scheduler := &cacheScheduler{stop: make(chan struct{}), done: make(chan struct{})}
	c.scheduler = scheduler
	interval := c.tickEvery
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer close(scheduler.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.tickScheduler(scheduler, c.now())
			case <-scheduler.stop:
				return
			}
		}
	}()
}

func (c *cacheControl) stopLocked() {
	if c.scheduler != nil {
		close(c.scheduler.stop)
		c.scheduler = nil
	}
}

func (c *cacheControl) stopIdleLocked() {
	if len(c.hot)+len(c.inflight)+len(c.pending) == 0 {
		c.stopLocked()
	}
}

func (c *cacheControl) tickScheduler(scheduler *cacheScheduler, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Idle shutdown and fresh demand may overlap. A retiring ticker must not
	// update the replacement scheduler or launch work on its behalf.
	if c.scheduler == scheduler {
		c.tickLocked(now)
	}
}

func (c *cacheControl) tick(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tickLocked(now)
}

func (c *cacheControl) tickLocked(now time.Time) {
	c.syncPolicyLocked()
	defer c.stopIdleLocked()
	if c.closed {
		return
	}
	c.launchPendingRefreshLocked(now)
	if c.options.Prefetch {
		c.launchDuePrefetchLocked(now)
	}
}

func (c *cacheControl) launchLocked(ctx context.Context, query *D.Msg, key string) bool {
	return c.launchRefreshLocked(ctx, query, key, nil)
}

func (c *cacheControl) launchRefreshLocked(ctx context.Context, query *D.Msg, key string, prefetch *cacheHotEntry) bool {
	if !c.validLocked(ctx) {
		return false
	}
	if _, exists := c.inflight[key]; exists {
		return false
	}
	if prefetch != nil && cachePendingRefreshCount.Load() != 0 {
		return false
	}
	select {
	case cacheBackgroundSlots <- struct{}{}:
	default:
		return false
	}
	job := cacheRefreshJob{key: key, ctx: ctx, query: query.Copy(), generation: c.generation, lifetime: c.lifetime, prefetch: prefetch}
	if prefetch != nil {
		c.beginPrefetchLocked(prefetch)
		job.revision, job.demand = prefetch.revision, prefetch.demand
	}
	c.inflight[key] = job.generation
	c.startLocked()
	c.wg.Add(1)
	go c.run(job)
	return true
}

func (c *cacheControl) run(job cacheRefreshJob) {
	defer c.wg.Done()
	defer func() {
		c.mu.Lock()
		<-cacheBackgroundSlots
		c.syncPolicyLocked()
		if c.inflight[job.key] == job.generation {
			delete(c.inflight, job.key)
		}
		c.finishPrefetchLocked(job)
		c.launchPendingRefreshLocked(c.now())
		c.stopIdleLocked()
		c.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.WithValue(job.ctx, cacheBackgroundKey{}, true), resolver.DefaultDNSTimeout)
	stop := contextutils.AfterFunc(job.lifetime, cancel)
	defer stop()
	defer cancel()
	if job.lifetime.Err() != nil || !c.active(job.ctx) {
		return
	}
	// Re-evaluate live rules before any network work, including REJECT. The
	// resulting key/pool may differ; exchangeWithoutCache writes its new key.
	ctx, reply, err := c.r.prepareDNSRouting(ctx, job.query)
	if err != nil || reply != nil {
		c.removeJobHot(job)
		return
	}
	if ctx.Err() != nil || job.lifetime.Err() != nil || !c.active(ctx) {
		return
	}
	newKey := c.r.cacheKey(ctx, job.query)
	if newKey != job.key {
		// Re-arm the new scope only upon real demand. Never let an old route's
		// hotness indefinitely drive queries in a newly selected policy scope.
		c.removeJobHot(job)
		if job.prefetch != nil {
			return
		}
	}
	if job.prefetch != nil && !c.r.directDNSQueryEligible(ctx, job.query) {
		c.removeJobHot(job)
		return
	}
	_, _ = c.fetch(ctx, job.query)
}

func (c *cacheControl) active(ctx context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncPolicyLocked()
	return c.validLocked(ctx)
}

func (c *cacheControl) removeJobHot(job cacheRefreshJob) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation == job.generation {
		if entry := c.hot[job.key]; entry != nil {
			if job.prefetch != nil && (entry != job.prefetch || entry.demand != job.demand) {
				return
			}
			c.removeHotLocked(entry)
		}
	}
}

func (c *cacheControl) syncPolicyLocked() {
	if epoch := tunnel.DNSRoutingEpoch(); epoch != c.epoch && !c.closed {
		c.epoch = epoch
		c.resetLocked()
	}
}

func (c *cacheControl) resetLocked() {
	c.cancel()
	c.generation++
	c.lifetime, c.cancel = context.WithCancel(context.Background())
	c.hot = make(map[string]*cacheHotEntry)
	c.lru.Init()
	c.ready = nil
	c.hotBytes = 0
	c.inflight = make(map[string]uint64)
	c.clearPendingRefreshLocked()
	c.selections = nil
	c.clearDualStackSamplesLocked()
	c.stopIdleLocked()
}

func (c *cacheControl) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.resetLocked()
	}
	if c.r.cache != nil {
		c.r.cache.Clear()
	}
}

func (c *cacheControl) Close() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.cancel()
		c.generation++
		c.hot = make(map[string]*cacheHotEntry)
		c.lru.Init()
		c.ready = nil
		c.hotBytes = 0
		c.selections = nil
		c.clearPendingRefreshLocked()
		c.clearDualStackSamplesLocked()
		c.stopLocked()
	}
	c.mu.Unlock()
	c.wg.Wait()
}

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
}

const (
	cacheHotCapacity       = 1024
	cacheHotThreshold      = 3
	cacheHotWindow         = time.Minute
	cacheRefreshMinPeriod  = time.Second
	cacheRefreshRetryDelay = 10 * time.Second
)

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

type cacheHotEntry struct {
	key                    string
	ctx                    context.Context
	query                  *D.Msg
	window, lastSeen, next time.Time
	expires                time.Time
	hits                   int
	element                *list.Element
}

type cacheRefreshJob struct {
	key        string
	ctx        context.Context
	query      *D.Msg
	generation uint64
	lifetime   context.Context
}

type cacheScheduler struct {
	stop chan struct{}
	done chan struct{}
}

type cacheControl struct {
	r       *Resolver
	options CacheOptions

	mu         sync.Mutex
	generation uint64
	epoch      uint64
	lifetime   context.Context
	cancel     context.CancelFunc
	closed     bool
	scheduler  *cacheScheduler
	wg         sync.WaitGroup
	hot        map[string]*cacheHotEntry
	lru        list.List
	inflight   map[string]uint64

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
	ctx, cancel := context.WithCancel(context.Background())
	return &cacheControl{
		r: r, options: value, generation: 1, epoch: tunnel.DNSRoutingEpoch(),
		lifetime: ctx, cancel: cancel,
		hot: make(map[string]*cacheHotEntry), inflight: make(map[string]uint64),
		now: time.Now, tickEvery: time.Second, fetch: r.exchangeWithoutCache,
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

// Observe counts foreground demand only, after prepareDNSRouting selected its
// route/cache scope. Prefetch requires an actual source; infrastructure and
// anonymous internal lookups may still use request-triggered stale refreshes.
func (c *cacheControl) Observe(ctx context.Context, query *D.Msg, key string) {
	if !c.options.Prefetch || cacheBackground(ctx) || icontext.DNSBootstrap(ctx) || !icontext.DNSRoutingInbound(ctx) || query == nil || len(query.Question) != 1 {
		return
	}
	ctx, source := cacheRefreshContext(ctx)
	if !source {
		return
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncPolicyLocked()
	if !c.validLocked(ctx) {
		return
	}
	entry := c.hot[key]
	if entry == nil {
		if len(c.hot) >= cacheHotCapacity {
			c.removeHotLocked(c.lru.Back().Value.(*cacheHotEntry))
		}
		entry = &cacheHotEntry{key: key, window: now}
		entry.element = c.lru.PushFront(entry)
		c.hot[key] = entry
	} else {
		c.lru.MoveToFront(entry.element)
	}
	if now.Sub(entry.window) >= cacheHotWindow {
		entry.window, entry.hits = now, 0
	}
	if entry.hits < cacheHotThreshold {
		entry.hits++
	}
	entry.ctx, entry.query, entry.lastSeen = ctx, query.Copy(), now
	if entry.next.IsZero() && c.r.cache != nil {
		if _, expires, ok := c.r.cache.GetWithExpire(key); ok {
			c.scheduleLocked(entry, now, expires)
		}
	}
	c.startLocked()
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
	c.launchLocked(ctx, query, key)
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
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncPolicyLocked()
	if !c.validLocked(ctx) || message == nil || c.r.cache == nil {
		return false
	}
	putMsgToCache(c.r.cache, key, question, message)
	if entry := c.hot[key]; entry != nil {
		if _, expires, ok := c.r.cache.GetWithExpire(key); ok {
			c.scheduleLocked(entry, c.now(), expires)
		} else {
			c.removeHotLocked(entry)
			c.stopIdleLocked()
		}
	}
	return true
}

func (c *cacheControl) scheduleLocked(entry *cacheHotEntry, now, expires time.Time) {
	entry.expires = expires
	delay := expires.Sub(now)
	// DNS TTL is uint32 seconds; multiplying its duration by four can
	// overflow time.Duration for large, otherwise representable TTLs.
	delay -= delay / 5
	if delay < cacheRefreshMinPeriod {
		delay = cacheRefreshMinPeriod
	}
	entry.next = now.Add(delay)
}

func (c *cacheControl) startLocked() {
	if c.scheduler != nil || c.closed || len(c.hot)+len(c.inflight) == 0 {
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
	if len(c.hot)+len(c.inflight) == 0 {
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
	if c.closed || !c.options.Prefetch {
		return
	}
	for _, entry := range c.hot {
		if now.Sub(entry.lastSeen) >= cacheHotWindow {
			c.removeHotLocked(entry)
			continue
		}
		if entry.hits < cacheHotThreshold || entry.next.IsZero() || now.Before(entry.next) {
			continue
		}
		if c.launchLocked(entry.ctx, entry.query, entry.key) {
			// Errors cannot create a tight retry loop. A successful Store sets
			// a fresh deadline derived from the newly cached TTL instead.
			entry.next = now.Add(cacheRefreshRetryDelay)
		}
	}
}

func (c *cacheControl) launchLocked(ctx context.Context, query *D.Msg, key string) bool {
	if !c.validLocked(ctx) {
		return false
	}
	if _, exists := c.inflight[key]; exists {
		return false
	}
	select {
	case cacheBackgroundSlots <- struct{}{}:
	default:
		return false
	}
	job := cacheRefreshJob{key: key, ctx: ctx, query: query.Copy(), generation: c.generation, lifetime: c.lifetime}
	c.inflight[key] = job.generation
	c.startLocked()
	c.wg.Add(1)
	go c.run(job)
	return true
}

func (c *cacheControl) run(job cacheRefreshJob) {
	defer c.wg.Done()
	defer func() { <-cacheBackgroundSlots }()
	defer func() {
		c.mu.Lock()
		if c.inflight[job.key] == job.generation {
			delete(c.inflight, job.key)
		}
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
			c.removeHotLocked(entry)
		}
	}
}

func (c *cacheControl) removeHotLocked(entry *cacheHotEntry) {
	delete(c.hot, entry.key)
	c.lru.Remove(entry.element)
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
	c.inflight = make(map[string]uint64)
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
		c.stopLocked()
	}
	c.mu.Unlock()
	c.wg.Wait()
}

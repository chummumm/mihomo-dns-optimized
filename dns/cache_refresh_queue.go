package dns

import (
	"container/list"
	"context"
	"strings"
	"sync/atomic"
	"time"

	D "github.com/miekg/dns"
)

const (
	cachePendingRefreshLimit   = 32
	cachePendingRefreshBudget  = 256 << 10
	cachePendingRefreshMaxWait = 30 * time.Second
)

// A waiting stale-hit refresh takes priority over new proactive work, even
// when it belongs to another resolver. Existing work is allowed to finish;
// the next free slot is not repeatedly stolen by a hot prefetch controller.
var cachePendingRefreshCount atomic.Int64

type cachePendingRefresh struct {
	key     string
	request cacheRefreshRequest
	expires time.Time
	bytes   int
	element *list.Element
}

func (c *cacheControl) queueRefreshLocked(ctx context.Context, query *D.Msg, key string) {
	if len(c.pending) >= cachePendingRefreshLimit {
		return
	}
	request, ok := newCacheRefreshRequest(ctx, query)
	if !ok {
		return
	}
	bytes := request.bytes + len(key)
	if bytes > cachePrefetchEntryLimit || c.pendingBytes+bytes > cachePendingRefreshBudget {
		return
	}
	if c.pending == nil {
		c.pending = make(map[string]*cachePendingRefresh)
	}
	item := &cachePendingRefresh{
		key: strings.Clone(key), request: request,
		expires: c.now().Add(cachePendingRefreshMaxWait), bytes: bytes,
	}
	item.element = c.pendingOrder.PushBack(item)
	c.pending[key] = item
	c.pendingBytes += bytes
	cachePendingRefreshCount.Add(1)
	c.startLocked()
}

func (c *cacheControl) launchPendingRefreshLocked(now time.Time) {
	if c.closed {
		return
	}
	for c.pendingOrder.Len() != 0 {
		item := c.pendingOrder.Front().Value.(*cachePendingRefresh)
		if !now.Before(item.expires) {
			c.removePendingRefreshLocked(item)
			continue
		}
		if _, running := c.inflight[item.key]; running {
			c.removePendingRefreshLocked(item)
			continue
		}
		if c.freshRefreshAnswerLocked(item.key, now) {
			// A foreground exchange or its later optimized response may have
			// satisfied this request while it waited for a background slot.
			c.removePendingRefreshLocked(item)
			continue
		}
		query := new(D.Msg)
		if err := query.Unpack(item.request.wire); err != nil {
			c.removePendingRefreshLocked(item)
			continue
		}
		if !c.launchLocked(item.request.context(c), query, item.key) {
			return
		}
		c.removePendingRefreshLocked(item)
	}
}

func (c *cacheControl) freshRefreshAnswerLocked(key string, now time.Time) bool {
	if c.r.cache == nil {
		return false
	}
	message, expires, exists := c.r.cache.GetWithExpire(key)
	if !exists || message == nil || message.Truncated || !expires.After(now) {
		return false
	}
	// Retrying an error or canceled exchange remains useful after a short
	// cached SERVFAIL; a complete positive or negative answer needs no retry.
	return message.Rcode == D.RcodeSuccess || message.Rcode == D.RcodeNameError
}

func (c *cacheControl) removePendingRefreshLocked(item *cachePendingRefresh) {
	delete(c.pending, item.key)
	c.pendingOrder.Remove(item.element)
	c.pendingBytes -= item.bytes
	cachePendingRefreshCount.Add(-1)
	if len(c.pending) == 0 {
		c.pending = nil
	}
}

func (c *cacheControl) clearPendingRefreshLocked() {
	cachePendingRefreshCount.Add(-int64(len(c.pending)))
	c.pending = nil
	c.pendingOrder.Init()
	c.pendingBytes = 0
}

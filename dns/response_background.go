package dns

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/contextutils"
	D "github.com/miekg/dns"
)

type dnsResponseContinuationKey struct{}

var errDNSResponseBudget = errors.New("DNS background selection budget ended")

// The existing exchange goroutine owns this lease. A successful handoff takes
// one bounded background slot before releasing its foreground slot; it creates
// no second resolver exchange, DNS request, or optimization deadline.
type dnsResponseContinuation struct {
	control *cacheControl
	key     string
	cancel  context.CancelFunc
	stop    func() bool
	budget  *time.Timer
	once    sync.Once
}

type dnsResponsePublisher struct {
	mu      sync.Mutex
	r       *Resolver
	ctx     context.Context
	query   *D.Msg
	key     string
	publish func(*dnsExchangeResult) bool
	release func()
	cancel  context.CancelCauseFunc
	lease   *dnsResponseContinuation
}

func (p *dnsResponsePublisher) offer(message *D.Msg) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lease != nil {
		return true
	}
	if p.r.cacheControl == nil || message == nil || p.ctx.Err() != nil || dnsQueryClosed(p.ctx) {
		return false
	}
	lease := p.r.cacheControl.beginResponseContinuation(p.ctx, p.key, p.cancel)
	if lease == nil {
		return false
	}
	writeContext := context.WithValue(p.ctx, dnsResponseContinuationKey{}, lease)
	adjusted := p.r.answerPolicy.apply(p.query, message)
	answer := ageDNSAnswerAfterPolicy(writeContext, message, adjusted)
	inheritDNSLocalAnswer(writeContext, answer, message)
	if !p.r.cacheControl.Store(writeContext, p.key, p.query.Question[0], answer) {
		lease.Close()
		return false
	}
	p.lease = lease
	local, _ := p.ctx.Value(dnsLocalAnswersKey{}).(*dnsLocalAnswers)
	// Publish the adjusted, unaged snapshot with its original receipt clock.
	// Each waiter ages once from that clock. Aging an already aged snapshot
	// again from publication would lose the fractional second at each step.
	// Every consumer receives a private copy, including late flight joiners.
	result := &dnsExchangeResult{Msg: adjusted.Copy(), local: local != nil && local.has(answer), receivedAt: dnsAnswerReceivedAt(writeContext, message)}
	p.release()
	return p.publish(result)
}

func (p *dnsResponsePublisher) storeContext(ctx context.Context) context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lease != nil {
		return context.WithValue(ctx, dnsResponseContinuationKey{}, p.lease)
	}
	return ctx
}

func (p *dnsResponsePublisher) Close() {
	p.mu.Lock()
	lease := p.lease
	p.mu.Unlock()
	lease.Close()
}

func (c *cacheControl) beginResponseContinuation(ctx context.Context, key string, cancel context.CancelCauseFunc) *dnsResponseContinuation {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncPolicyLocked()
	if !c.validLocked(ctx) || cacheBackground(ctx) || ctx.Err() != nil || dnsQueryClosed(ctx) {
		return nil
	}
	deadline, ok := ctx.Value(dnsSpeedCheckDeadlineKey{}).(time.Time)
	if !ok || !deadline.After(time.Now()) {
		return nil
	}
	select {
	case cacheBackgroundSlots <- struct{}{}:
	default:
		return nil
	}
	lease := &dnsResponseContinuation{control: c, key: key, cancel: func() { cancel(context.Canceled) }}
	lease.stop = contextutils.AfterFunc(c.lifetime, lease.cancel)
	lease.budget = time.AfterFunc(time.Until(deadline), func() { cancel(errDNSResponseBudget) })
	if c.selections == nil {
		c.selections = make(map[string]*dnsResponseContinuation)
	}
	if previous := c.selections[key]; previous != nil {
		previous.cancel()
	}
	c.selections[key] = lease
	c.wg.Add(1)
	return lease
}

func (l *dnsResponseContinuation) Close() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.stop()
		l.budget.Stop()
		l.control.mu.Lock()
		if l.control.selections[l.key] == l {
			delete(l.control.selections, l.key)
		}
		l.control.mu.Unlock()
		<-cacheBackgroundSlots
		l.control.wg.Done()
	})
}

// Called with the same mutex as generation-aware Store. A newer normal write
// revokes older optimization work; an old revoked continuation can never
// overwrite a newer reply, even when its upstream ignores cancellation.
func (c *cacheControl) acceptResponseWriteLocked(ctx context.Context, key string) bool {
	lease, _ := ctx.Value(dnsResponseContinuationKey{}).(*dnsResponseContinuation)
	current := c.selections[key]
	if lease != nil {
		return current == lease && (ctx.Err() == nil || errors.Is(context.Cause(ctx), errDNSResponseBudget)) && !dnsQueryClosed(ctx)
	}
	if current != nil {
		delete(c.selections, key)
		current.cancel()
	}
	return !dnsQueryClosed(ctx)
}

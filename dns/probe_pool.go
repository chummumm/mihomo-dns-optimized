package dns

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/contextutils"
)

var errDNSProbeBusy = errors.New("DIRECT probe queue full")
var errDNSProbeClosed = errors.New("DIRECT probe pool closed")

type dnsProbeWork func(context.Context) (time.Duration, error)
type dnsProbeDeliver func(time.Duration, error)
type dnsProbeSample struct {
	rtt     time.Duration
	expires time.Time
	serial  uint64
}
type dnsProbeRingEntry struct {
	key    string
	serial uint64
}
type dnsProbeTask struct {
	key     string
	share   bool
	ctx     context.Context
	cancel  context.CancelFunc
	stop    func() bool
	work    dnsProbeWork
	waiters map[uint64]dnsProbeDeliver
}
type dnsProbeTicket struct {
	pool *dnsProbePool
	task *dnsProbeTask
	id   uint64
}
type dnsProbePool struct {
	mu              sync.Mutex
	ctx             context.Context
	cancel          context.CancelFunc
	workers         int
	timeout         time.Duration
	started, closed bool
	serial          uint64
	jobs            chan *dnsProbeTask
	tasks           map[string]*dnsProbeTask
	samples         map[string]dnsProbeSample
	ring            [1024]dnsProbeRingEntry
	ringPos         int
	wg              sync.WaitGroup
}

func newDNSProbePool(workers, queue int, timeout time.Duration) *dnsProbePool {
	ctx, cancel := context.WithCancel(context.Background())
	return &dnsProbePool{ctx: ctx, cancel: cancel, workers: workers, timeout: timeout,
		jobs: make(chan *dnsProbeTask, queue), tasks: make(map[string]*dnsProbeTask), samples: make(map[string]dnsProbeSample)}
}

// Only a frozen real DIRECT plan gives a safe cross-query probe identity.
// Arbitrary custom callbacks (including tests) deliberately receive no reuse.
func dnsProbeScope(ctx context.Context, ip netip.Addr) string {
	r := queryRoute(ctx)
	if r == nil || r.plan == nil || r.err != nil {
		return ""
	}
	return fmt.Sprintf("%s|dscp:%d|%s", r.plan.TransportKey(), r.origin.DSCP, ip)
}

func (p *dnsProbePool) submit(ctx context.Context, key string, work dnsProbeWork, deliver dnsProbeDeliver) (*dnsProbeTicket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errDNSProbeClosed
	}
	share := key != ""
	if share {
		if sample, ok := p.samples[key]; ok {
			if time.Now().Before(sample.expires) {
				p.mu.Unlock()
				deliver(sample.rtt, nil)
				return nil, nil
			}
			delete(p.samples, key)
		}
	}
	p.serial++
	id := p.serial
	if !share {
		key = fmt.Sprintf("private:%d", id)
	}
	if task := p.tasks[key]; task != nil && task.ctx.Err() == nil {
		task.waiters[id] = deliver
		p.mu.Unlock()
		return &dnsProbeTicket{p, task, id}, nil
	}
	if p.tasks[key] != nil {
		key = fmt.Sprintf("expired-retry:%d", id)
		share = false
	}
	// Both queue and running task identities are bounded. A failed admission
	// only skips a speed probe; the original valid DNS answer remains usable.
	if len(p.tasks) >= cap(p.jobs)+p.workers {
		p.mu.Unlock()
		return nil, errDNSProbeBusy
	}
	budget := p.timeout
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < budget {
		budget = time.Until(deadline)
	}
	jobCtx, cancel := context.WithTimeout(contextutils.WithoutCancel(ctx), budget)
	task := &dnsProbeTask{key: key, share: share, ctx: jobCtx, cancel: cancel,
		work: work, waiters: map[uint64]dnsProbeDeliver{id: deliver}}
	task.stop = context.AfterFunc(p.ctx, cancel)
	if !p.started {
		p.started = true
		for i := 0; i < p.workers; i++ {
			p.wg.Add(1)
			go p.worker()
		}
	}
	select {
	case p.jobs <- task:
		p.tasks[key] = task
		p.mu.Unlock()
		return &dnsProbeTicket{p, task, id}, nil
	default:
		p.mu.Unlock()
		task.stop()
		cancel()
		return nil, errDNSProbeBusy
	}
}

func (t *dnsProbeTicket) Release() {
	if t == nil {
		return
	}
	p := t.pool
	p.mu.Lock()
	if p.tasks[t.task.key] == t.task {
		delete(t.task.waiters, t.id)
		if len(t.task.waiters) == 0 {
			t.task.cancel()
		}
	}
	p.mu.Unlock()
}

func (p *dnsProbePool) worker() {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case task := <-p.jobs:
			rtt, err := time.Duration(0), task.ctx.Err()
			if err == nil {
				rtt, err = task.work(task.ctx)
			}
			p.finish(task, rtt, err)
		}
	}
}

func (p *dnsProbePool) finish(task *dnsProbeTask, rtt time.Duration, err error) {
	task.stop()
	task.cancel()
	p.mu.Lock()
	if p.tasks[task.key] != task {
		p.mu.Unlock()
		return
	}
	delete(p.tasks, task.key)
	waiters := task.waiters
	task.waiters = nil
	if !p.closed && task.share && err == nil && rtt >= 0 && len(waiters) > 0 {
		p.serial++
		previous := p.ring[p.ringPos]
		if old, ok := p.samples[previous.key]; ok && old.serial == previous.serial {
			delete(p.samples, previous.key)
		}
		p.samples[task.key] = dnsProbeSample{rtt: rtt, expires: time.Now().Add(time.Second), serial: p.serial}
		p.ring[p.ringPos] = dnsProbeRingEntry{task.key, p.serial}
		p.ringPos = (p.ringPos + 1) % len(p.ring)
	}
	p.mu.Unlock()
	for _, deliver := range waiters {
		deliver(rtt, err)
	}
}

func (p *dnsProbePool) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.wg.Wait()
		return
	}
	p.closed = true
	p.cancel()
	tasks := p.tasks
	p.tasks = make(map[string]*dnsProbeTask)
	p.samples = nil
	var deliveries []dnsProbeDeliver
	for _, task := range tasks {
		task.stop()
		task.cancel()
		for _, f := range task.waiters {
			deliveries = append(deliveries, f)
		}
		task.waiters = nil
	}
	p.mu.Unlock()
	for _, deliver := range deliveries {
		deliver(0, errDNSProbeClosed)
	}
	p.wg.Wait()
	for {
		select {
		case <-p.jobs:
		default:
			return
		}
	}
}

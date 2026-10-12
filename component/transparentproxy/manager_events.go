package transparentproxy

import (
	"errors"
	"time"
)

const (
	networkEventQuietPeriod = 50 * time.Millisecond
	networkEventMaxDelay    = 250 * time.Millisecond
)

// Healthy operation has no ticker. A burst waits for a short quiet period,
// bounded from its first event so continuous advertisements cannot starve an
// update. Protection requests and lifecycle kicks bypass this window.
func (m *manager) run() {
	defer close(m.done)
	var retry, batch <-chan time.Time
	var retryTimer, batchTimer *time.Timer
	var burstStart time.Time
	var failedEvents <-chan systemEvent
	backoff := 100 * time.Millisecond
	defer func() {
		stopManagerTimer(retryTimer)
		stopManagerTimer(batchTimer)
	}()
	for {
		m.mu.Lock()
		events := m.events
		closed := m.closed || m.closing
		m.mu.Unlock()
		if closed {
			return
		}
		if events == failedEvents {
			events = nil
		}
		var observationError error
		select {
		case <-m.ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				event.Err = errors.New("network notification subscription closed")
			}
			if event.Err != nil {
				event.Urgent = true
			}
			m.mu.Lock()
			if m.events != events {
				m.mu.Unlock()
				continue
			}
			if event.Err != nil {
				failedEvents = events
			}
			if retry != nil {
				if !event.Urgent {
					m.mu.Unlock()
					continue
				}
				if m.uncertain {
					if m.closed || m.closing || m.ctx.Err() != nil {
						m.mu.Unlock()
						return
					}
					// An external flush may remove an earlier fence during
					// recovery. Repair protection immediately, leaving complete
					// reads and error reporting on the existing retry schedule.
					// Its own notifications only perform an unchanged-state check.
					_ = m.fenceCaptureLocked()
					m.mu.Unlock()
					continue
				}
			}
			m.mu.Unlock()
			observationError = event.Err
			if !event.Urgent {
				now := time.Now()
				if burstStart.IsZero() {
					burstStart = now
				}
				deadline := burstStart.Add(networkEventMaxDelay)
				if quiet := now.Add(networkEventQuietPeriod); quiet.Before(deadline) {
					deadline = quiet
				}
				// Check the deadline even while receiving events continuously;
				// timer selection fairness is not part of the latency bound.
				if delay := deadline.Sub(now); delay > 0 {
					resetManagerTimer(&batchTimer, delay)
					batch = batchTimer.C
					continue
				}
			}
		case <-m.kick:
			if retry != nil {
				m.mu.Lock()
				stillRecovering := m.uncertain || m.retryNeeded
				m.mu.Unlock()
				if stillRecovering {
					continue
				}
			}
		case <-batch:
		case <-retry:
		}
		stopManagerTimer(batchTimer)
		stopManagerTimer(retryTimer)
		batch, retry, burstStart = nil, nil, time.Time{}
		m.mu.Lock()
		if m.closed || m.closing || m.ctx.Err() != nil {
			m.mu.Unlock()
			return
		}
		// A ready timer/kick can win select while a failure is already queued.
		// Fold a pre-snapshot invalidation in now so that failure protection
		// runs first. Notifications arriving during the snapshot remain queued.
		if m.events != failedEvents {
			select {
			case pending, ok := <-m.events:
				if !ok {
					pending.Err = errors.New("network notification subscription closed")
				}
				if pending.Err != nil {
					observationError = pending.Err
					failedEvents = m.events
				}
			default:
			}
		}
		if observationError != nil {
			m.loseObservationLocked(observationError)
		}
		err := m.reconcileLocked()
		if err != nil {
			// A failed complete read or transaction never licenses destructive
			// cleanup. Resume only after resubscribing and reading fresh state.
			m.loseObservationLocked(err)
		}
		needRetry := m.retryNeeded
		// All lifecycle kicks are issued under this mutex. The completed
		// apply covers them, including its own subscription-change wakeup.
		// Network notifications stay queued so changes during the dump retry.
		select {
		case <-m.kick:
		default:
		}
		m.mu.Unlock()
		if err != nil || needRetry {
			resetManagerTimer(&retryTimer, backoff)
			retry = retryTimer.C
			backoff *= 2
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
		} else {
			backoff = 100 * time.Millisecond
		}
	}
}

func stopManagerTimer(timer *time.Timer) {
	if timer != nil && !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func resetManagerTimer(timer **time.Timer, delay time.Duration) {
	if *timer == nil {
		*timer = time.NewTimer(delay)
		return
	}
	stopManagerTimer(*timer)
	(*timer).Reset(delay)
}

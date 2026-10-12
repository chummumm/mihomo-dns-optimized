package transparentproxy

import "sync"

// A bounded invalidation queue may merge ordinary changes, but must never
// replace a protection request or an observation failure with an ordinary one.
// Producers serialize here; consumers only receive from events. The owner
// closes events after all producers have stopped.
type systemEventQueue struct {
	mu     sync.Mutex
	events chan systemEvent
	failed bool
}

func newSystemEventQueue() *systemEventQueue {
	return &systemEventQueue{events: make(chan systemEvent, 1)}
}

func (q *systemEventQueue) publish(event systemEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failed {
		return
	}
	select {
	case queued := <-q.events:
		event.Urgent = event.Urgent || queued.Urgent
		if event.Err == nil {
			event.Err = queued.Err
		}
	default:
	}
	q.failed = event.Err != nil
	event.Urgent = event.Urgent || q.failed
	q.events <- event
}

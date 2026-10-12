package transparentproxy

import (
	"errors"
	"sync"
	"testing"
)

func TestSystemEventQueueNeverDowngradesProtectionOrFailure(t *testing.T) {
	queue := newSystemEventQueue()
	for i := 0; i < 64; i++ {
		queue.publish(systemEvent{})
	}
	queue.publish(systemEvent{Urgent: true})
	for i := 0; i < 64; i++ {
		queue.publish(systemEvent{})
	}
	if event := <-queue.events; !event.Urgent || event.Err != nil {
		t.Fatalf("ordinary notifications replaced protection: %+v", event)
	}
	select {
	case event := <-queue.events:
		t.Fatalf("burst was not merged: %+v", event)
	default:
	}
	queue.publish(systemEvent{Urgent: true})
	lost := errors.New("notification overrun")
	queue.publish(systemEvent{Err: lost})
	queue.publish(systemEvent{})
	if event := <-queue.events; !event.Urgent || !errors.Is(event.Err, lost) {
		t.Fatalf("queued observation failure lost: %+v", event)
	}
	queue.publish(systemEvent{})
	select {
	case event := <-queue.events:
		t.Fatalf("failed subscription produced a trusted invalidation: %+v", event)
	default:
	}
}

func TestSystemEventQueueConcurrentPublishersPreserveUrgency(t *testing.T) {
	queue := newSystemEventQueue()
	var producers sync.WaitGroup
	for i := 0; i < 8; i++ {
		producers.Add(1)
		go func(urgent bool) {
			defer producers.Done()
			for j := 0; j < 64; j++ {
				queue.publish(systemEvent{Urgent: urgent})
			}
		}(i == 3)
	}
	producers.Wait()
	if event := <-queue.events; !event.Urgent {
		t.Fatal("concurrent ordinary publishers erased the urgent invalidation")
	}
}

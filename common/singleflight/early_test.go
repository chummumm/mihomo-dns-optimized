package singleflight

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func receiveEarlyTest[T any](t *testing.T, channel <-chan Result[T]) Result[T] {
	t.Helper()
	select {
	case result := <-channel:
		return result
	case <-time.After(time.Second):
		t.Fatal("singleflight waiter did not finish")
		return Result[T]{}
	}
}

func TestDoChanEarlyPreservesNormalWaitersAndPublishesOnce(t *testing.T) {
	var group Group[int]
	finish := make(chan struct{})
	early := group.DoChanEarly("query", func(publish func(int) bool) (int, error) {
		if !publish(1) || publish(99) {
			t.Error("early result must be published exactly once")
		}
		<-finish
		return 2, nil
	})
	if result := receiveEarlyTest(t, early); result.Val != 1 || result.Err != nil || !result.Shared {
		t.Fatalf("initial result: %+v", result)
	}
	normal := group.DoChan("query", func() (int, error) { t.Error("duplicate execution"); return 9, nil })
	late := group.DoChanEarly("query", func(func(int) bool) (int, error) { t.Error("duplicate execution"); return 9, nil })
	if result := receiveEarlyTest(t, late); result.Val != 1 || !result.Shared {
		t.Fatalf("late early waiter: %+v", result)
	}
	select {
	case result := <-normal:
		t.Fatalf("ordinary DoChan received an early result: %+v", result)
	default:
	}
	close(finish)
	if result := receiveEarlyTest(t, normal); result.Val != 2 || result.Err != nil {
		t.Fatalf("normal final result: %+v", result)
	}
	select {
	case result := <-early:
		t.Fatalf("early waiter received a second result: %+v", result)
	default:
	}
	if result := receiveEarlyTest(t, group.DoChan("query", func() (int, error) { return 3, nil })); result.Val != 3 {
		t.Fatal("completed call remained in flight")
	}
}

func TestDoChanEarlyStoreResultUsesFinalValue(t *testing.T) {
	group := Group[int]{StoreResult: true}
	finish := make(chan struct{})
	finalErr := errors.New("final failure")
	first := group.DoChanEarly("query", func(publish func(int) bool) (int, error) {
		publish(1)
		<-finish
		return 2, finalErr
	})
	if result := receiveEarlyTest(t, first); result.Val != 1 || result.Err != nil {
		t.Fatalf("first: %+v", result)
	}
	normal := group.DoChan("query", func() (int, error) { t.Error("duplicate"); return 0, nil })
	close(finish)
	if result := receiveEarlyTest(t, normal); result.Val != 2 || result.Err != finalErr {
		t.Fatalf("final: %+v", result)
	}
	for _, early := range []bool{false, true} {
		var channel <-chan Result[int]
		if early {
			channel = group.DoChanEarly("query", func(func(int) bool) (int, error) { t.Error("duplicate"); return 0, nil })
		} else {
			channel = group.DoChan("query", func() (int, error) { t.Error("duplicate"); return 0, nil })
		}
		if result := receiveEarlyTest(t, channel); result.Val != 2 || result.Err != finalErr {
			t.Fatalf("retained call lost its final value: %+v", result)
		}
	}
	if value, err, shared := group.Do("query", func() (int, error) { t.Error("duplicate"); return 0, nil }); value != 2 || err != finalErr || !shared {
		t.Fatalf("Do did not retain final result: %v, %v, %v", value, err, shared)
	}
	group.Forget("query")
	if value, _, _ := group.Do("query", func() (int, error) { return 3, nil }); value != 3 {
		t.Fatal("Forget retained an old result")
	}
	group.Reset()
	if value, _, _ := group.Do("query", func() (int, error) { return 4, nil }); value != 4 {
		t.Fatal("Reset retained an old result")
	}
}

func TestDoChanEarlyForgetAndResetCannotPublishIntoReplacement(t *testing.T) {
	for _, reset := range []bool{false, true} {
		var group Group[int]
		ready, proceed := make(chan struct{}), make(chan struct{})
		old := group.DoChanEarly("query", func(publish func(int) bool) (int, error) {
			close(ready)
			<-proceed
			if publish(1) {
				t.Error("forgotten call published an early value")
			}
			return 2, nil
		})
		<-ready
		if reset {
			group.Reset()
		} else {
			group.Forget("query")
		}
		replacement := group.DoChanEarly("query", func(func(int) bool) (int, error) { return 3, nil })
		if result := receiveEarlyTest(t, replacement); result.Val != 3 {
			t.Fatal(result)
		}
		close(proceed)
		if result := receiveEarlyTest(t, old); result.Val != 2 {
			t.Fatal(result)
		}
	}
}

func TestDoChanEarlyConcurrentWaitersAndCompletion(t *testing.T) {
	var group Group[int]
	var calls atomic.Int32
	ready, proceed := make(chan struct{}), make(chan struct{})
	first := group.DoChanEarly("query", func(publish func(int) bool) (int, error) {
		calls.Add(1)
		close(ready)
		<-proceed
		publish(1)
		return 2, nil
	})
	<-ready
	var wait sync.WaitGroup
	for i := 0; i < 64; i++ {
		channel := group.DoChanEarly("query", func(func(int) bool) (int, error) { calls.Add(1); return 9, nil })
		wait.Add(1)
		go func() {
			defer wait.Done()
			if result := receiveEarlyTest(t, channel); result.Val != 1 {
				t.Errorf("bad early result: %+v", result)
			}
		}()
	}
	close(proceed)
	if result := receiveEarlyTest(t, first); result.Val != 1 {
		t.Fatal(result)
	}
	wait.Wait()
	if calls.Load() != 1 {
		t.Fatalf("ran %d calls", calls.Load())
	}
}

func TestDoChanEarlyRetainedDoCompletionWithConcurrentLateWaiters(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		group := Group[int]{StoreResult: true}
		var calls atomic.Int32
		ready, finish := make(chan struct{}), make(chan struct{})
		original := make(chan Result[int], 1)
		go func() {
			value, err, shared := group.Do("retained", func() (int, error) {
				calls.Add(1)
				close(ready)
				<-finish
				return 7, nil
			})
			original <- Result[int]{value, err, shared}
		}()
		<-ready
		var wait sync.WaitGroup
		for waiter := 0; waiter < 16; waiter++ {
			waiter := waiter
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-finish
				duplicate := func() (int, error) { calls.Add(1); return 99, nil }
				var got Result[int]
				if waiter%2 == 0 {
					got.Val, got.Err, got.Shared = group.Do("retained", duplicate)
				} else {
					got = receiveEarlyTest(t, group.DoChan("retained", duplicate))
				}
				if got.Val != 7 || got.Err != nil || !got.Shared {
					t.Errorf("late retained waiter: %+v", got)
				}
			}()
		}
		close(finish)
		if got := receiveEarlyTest(t, original); got.Val != 7 || got.Err != nil {
			t.Fatalf("original retained Do: %+v", got)
		}
		wait.Wait()
		if calls.Load() != 1 {
			t.Fatalf("retained Do executed %d times", calls.Load())
		}
	}
}

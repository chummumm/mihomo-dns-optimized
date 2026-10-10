package lru

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestLRUCloneFilteredPreservesOrderAndExpiration(t *testing.T) {
	source := New[string, string](WithSize[string, string](3))
	target := New[string, string](WithSize[string, string](3))
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	source.SetWithExpire("first", "allowed-one", expires)
	source.SetWithExpire("blocked", "excluded", expires.Add(time.Second))
	source.SetWithExpire("last", "allowed-two", expires.Add(2*time.Second))
	source.Get("first")
	target.Set("obsolete", "old target entry")

	source.CloneToFiltered(target, func(_ string, value string) bool {
		return value != "excluded"
	})
	var order []string
	for e := target.lru.Front(); e != nil; e = e.Next() {
		order = append(order, e.Value.key)
	}
	if !reflect.DeepEqual(order, []string{"last", "first"}) {
		t.Fatalf("copied LRU order = %v", order)
	}
	if target.Exist("blocked") || target.Exist("obsolete") {
		t.Fatal("filtered snapshot retained an excluded or obsolete entry")
	}
	if !source.Exist("blocked") {
		t.Fatal("copy changed the source cache")
	}
	if value, expiry, ok := target.GetWithExpire("first"); !ok || value != "allowed-one" || !expiry.Equal(expires) {
		t.Fatalf("copied entry = %q, %v, %v", value, expiry, ok)
	}
	if _, expiry, ok := target.GetWithExpire("last"); !ok || !expiry.Equal(expires.Add(2*time.Second)) {
		t.Fatalf("copied last entry expiration = %v, found = %v", expiry, ok)
	}

	// The reads above made "last" most recent. Later insertions must follow
	// normal LRU eviction in the copied cache.
	target.Set("new-one", "one")
	target.Set("new-two", "two")
	if target.Exist("first") || !target.Exist("last") {
		t.Fatal("copied cache did not preserve subsequent LRU eviction behavior")
	}
}

func TestLRUCloneEntriesAreIndependent(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		t.Run(map[bool]string{false: "all", true: "filtered"}[filtered], func(t *testing.T) {
			source := New[string, string]()
			target := New[string, string]()
			expires := time.Now().Add(time.Hour).Truncate(time.Second)
			source.SetWithExpire("key", "original", expires)
			if filtered {
				source.CloneToFiltered(target, func(_, _ string) bool { return true })
			} else {
				source.CloneTo(target)
			}
			source.SetWithExpire("key", "late source update", expires.Add(time.Hour))
			if value, expiry, ok := target.GetWithExpire("key"); !ok || value != "original" || !expiry.Equal(expires) {
				t.Fatalf("source mutation reached copied cache: %q %v %v", value, expiry, ok)
			}
			target.Set("key", "new target update")
			if value, _, ok := source.GetWithExpire("key"); !ok || value != "late source update" {
				t.Fatalf("target mutation reached source cache: %q %v", value, ok)
			}
		})
	}
}

func TestLRUCloneSelf(t *testing.T) {
	cache := New[string, string]()
	cache.Set("keep", "allowed")
	cache.Set("drop", "excluded")
	done := make(chan struct{})
	go func() {
		cache.CloneTo(cache)
		cache.CloneToFiltered(cache, func(key, _ string) bool { return key == "keep" })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("self-copy deadlocked")
	}
	if value, ok := cache.Get("keep"); !ok || value != "allowed" || cache.Exist("drop") {
		t.Fatal("self-copy lost allowed entries or retained excluded entries")
	}
}

func TestLRUCloneConcurrentCopiesAndWrites(t *testing.T) {
	a := New[int, int](WithSize[int, int](16))
	b := New[int, int](WithSize[int, int](16))
	for i := 0; i < 16; i++ {
		a.Set(i, i)
		b.Set(i, i)
	}
	var wg sync.WaitGroup
	for _, pair := range [][2]*LruCache[int, int]{{a, b}, {b, a}} {
		source, target := pair[0], pair[1]
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				source.CloneTo(target)
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				source.Set(i%16, i)
				source.Get(i % 16)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("opposite-direction copies deadlocked")
	}
}

package routearc

import "testing"

func TestARCDeleteResidentAndGhost(t *testing.T) {
	cache := New[string, int](WithSize[string, int](2))
	cache.Set("frequent", 1)
	cache.Get("frequent")
	cache.Set("ghost", 2)
	cache.Set("recent", 3)
	if ent := cache.cache["ghost"]; ent == nil || !ent.ghost {
		t.Fatal("test did not create ARC history")
	}
	before := cache.Len()
	cache.Delete("ghost")
	if _, exists := cache.cache["ghost"]; exists || cache.Len() != before {
		t.Fatal("deleting history changed resident count or retained the key")
	}
	cache.Delete("frequent")
	cache.Delete("frequent")
	cache.Delete("missing")
	if _, ok := cache.Get("frequent"); ok || cache.Len() != before-1 {
		t.Fatal("resident deletion did not invalidate exactly once")
	}
	cache.Set("frequent", 4)
	if value, ok := cache.Get("frequent"); !ok || value != 4 {
		t.Fatal("deleted key could not be reinserted")
	}
}

func TestARCDeleteEmptyFrequencyListStillAllowsReplacement(t *testing.T) {
	cache := New[string, int](WithSize[string, int](2))
	cache.Set("frequent", 1)
	cache.Get("frequent")
	cache.Set("old", 2)
	cache.Set("recent", 3)
	// An adaptive cache may prefer recent entries after the last frequent
	// resident is explicitly invalidated. Replacement still needs a victim.
	cache.p = cache.c
	cache.Delete("frequent")
	cache.Set("new", 4)
	if value, ok := cache.Get("new"); !ok || value != 4 {
		t.Fatal("replacement after invalidation lost the new value")
	}
}

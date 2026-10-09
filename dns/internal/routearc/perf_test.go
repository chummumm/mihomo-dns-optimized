package routearc

import "testing"

func TestDNSPerfARCRepeatedHitDoesNotAllocate(t *testing.T) {
	c := New(WithSize[string, int](8))
	c.Set("hot", 1)
	c.Get("hot")
	got := testing.AllocsPerRun(1000, func() {
		value, ok := c.Get("hot")
		if !ok || value != 1 {
			panic("lost cached value")
		}
	})
	if got != 0 {
		t.Fatalf("same-list cache hit allocated %v objects", got)
	}
}

package sing_tun

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/sing/common/control"
)

func monitoredInterface(addresses ...string) *control.Interface {
	result := &control.Interface{
		Index:        2,
		Name:         "eth0",
		MTU:          1500,
		HardwareAddr: net.HardwareAddr{0x02, 0, 0, 0, 0, 1},
		Flags:        net.FlagUp | net.FlagRunning | net.FlagBroadcast | net.FlagMulticast,
	}
	for _, address := range addresses {
		result.Addresses = append(result.Addresses, netip.MustParsePrefix(address))
	}
	return result
}

func TestDefaultInterfaceChangeHandlerSourceChanges(t *testing.T) {
	const (
		ipv4       = "192.0.2.7/24"
		publicIPv6 = "2001:db8:1::1234/64"
		private6   = "fd00:1::1234/64"
		linkLocal6 = "fe80::1234/64"
	)
	baseline := []string{ipv4, publicIPv6, private6, linkLocal6}
	tests := []struct {
		name      string
		before    []string
		after     []string
		change    func(*control.Interface)
		event     int
		wantReset bool
	}{
		{name: "repeated RA", after: baseline},
		{name: "reordered and duplicate addresses", after: []string{linkLocal6, private6, ipv4, publicIPv6, publicIPv6}},
		{name: "temporary public IPv6 rotates within prefix", after: []string{ipv4, "2001:db8:1::5678/64", private6, linkLocal6}},
		{name: "ULA rotates to another prefix", after: []string{ipv4, publicIPv6, "fd00:2::5678/64", linkLocal6}},
		{name: "ULA removed while public IPv6 remains", after: []string{ipv4, publicIPv6, linkLocal6}},
		{name: "ULA added", after: append(append([]string{}, baseline...), "fd00:2::5678/64")},
		{name: "link-local rotates", after: []string{ipv4, publicIPv6, private6, "fe80::5678/64"}},
		{name: "link-local removed", after: []string{ipv4, publicIPv6, private6}},
		{name: "IPv4 added", after: append(append([]string{}, baseline...), "192.0.2.8/24")},
		{name: "public IPv6 prefix added", after: append(append([]string{}, baseline...), "2001:db8:2::1234/64")},
		{name: "new IPv6 family", before: []string{ipv4}, after: []string{ipv4, publicIPv6}},
		{name: "new IPv4 family", before: []string{publicIPv6}, after: []string{ipv4, publicIPv6}},
		{name: "IPv4 mask changes without losing source", after: []string{"192.0.2.7/25", publicIPv6, private6, linkLocal6}},
		{name: "IPv4 representation changes", after: []string{"::ffff:192.0.2.7/120", publicIPv6, private6, linkLocal6}},
		{name: "IPv4 source removed", after: []string{publicIPv6, private6, linkLocal6}, wantReset: true},
		{name: "IPv4 replaced within prefix", after: []string{"192.0.2.8/24", publicIPv6, private6, linkLocal6}, wantReset: true},
		{name: "one of multiple IPv4 sources removed", before: []string{ipv4, "192.0.2.8/24", publicIPv6}, after: []string{"192.0.2.8/24", publicIPv6}, wantReset: true},
		{name: "public IPv6 becomes ULA-only", after: []string{ipv4, private6, linkLocal6}, wantReset: true},
		{name: "public IPv6 becomes link-local-only", after: []string{ipv4, linkLocal6}, wantReset: true},
		{name: "public IPv6 prefix replaced", after: []string{ipv4, "2001:db8:2::1234/64", private6, linkLocal6}, wantReset: true},
		{name: "one of multiple public prefixes removed", before: []string{ipv4, publicIPv6, "2001:db8:2::1234/64"}, after: []string{ipv4, "2001:db8:2::1234/64"}, wantReset: true},
		{name: "IPv6 family lost", after: []string{ipv4}, wantReset: true},
		{name: "ULA-only network rotates", before: []string{ipv4, private6, linkLocal6}, after: []string{ipv4, "fd00:2::5678/64", linkLocal6}},
		{name: "last ULA-only connectivity lost", before: []string{ipv4, private6, linkLocal6}, after: []string{ipv4, linkLocal6}, wantReset: true},
		{name: "link-local-only address removed", before: []string{ipv4, linkLocal6}, after: []string{ipv4}},
		{name: "irrelevant non-unicast removed", before: []string{ipv4, "ff02::1/128", "::/128", "0.0.0.0/32"}, after: []string{ipv4}},
		{name: "interface index changes", change: func(i *control.Interface) { i.Index++ }, wantReset: true},
		{name: "interface name changes", change: func(i *control.Interface) { i.Name = "eth1" }, wantReset: true},
		{name: "interface MAC changes", change: func(i *control.Interface) { i.HardwareAddr[5]++ }, wantReset: true},
		{name: "interface goes down", change: func(i *control.Interface) { i.Flags &^= net.FlagUp }, wantReset: true},
		{name: "interface loses carrier", change: func(i *control.Interface) { i.Flags &^= net.FlagRunning }, wantReset: true},
		{name: "point-to-point mode changes", change: func(i *control.Interface) { i.Flags |= net.FlagPointToPoint }, wantReset: true},
		{name: "broadcast capability changes", change: func(i *control.Interface) { i.Flags &^= net.FlagBroadcast }},
		{name: "multicast capability changes", change: func(i *control.Interface) { i.Flags &^= net.FlagMulticast }},
		{name: "MTU decreases", change: func(i *control.Interface) { i.MTU = 1280 }, wantReset: true},
		{name: "MTU increases", change: func(i *control.Interface) { i.MTU = 9000 }, wantReset: true},
		{name: "Android VPN policy changes", event: 1, wantReset: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := baseline
			if test.before != nil {
				before = test.before
			}
			after := before
			if test.after != nil {
				after = test.after
			}
			var flushed, resets int
			handler := &defaultInterfaceChangeHandler{
				flushCache:       func() { flushed++ },
				resetConnections: func() { resets++ },
			}
			handler.Update(monitoredInterface(before...), 0)
			current := monitoredInterface(after...)
			if test.change != nil {
				test.change(current)
			}
			handler.Update(current, test.event)
			wantResets := 1 // Initial discovery establishes the exit.
			if test.wantReset {
				wantResets++
			}
			if resets != wantResets || flushed != 2 {
				t.Fatalf("resets=%d, cache refreshes=%d; want resets=%d, refreshes=2", resets, flushed, wantResets)
			}
		})
	}
}

func TestDefaultInterfaceChangeHandlerRemembersSourcesAddedWithoutReset(t *testing.T) {
	var flushed, resets int
	handler := &defaultInterfaceChangeHandler{
		flushCache:       func() { flushed++ },
		resetConnections: func() { resets++ },
	}
	handler.Update(monitoredInterface("192.0.2.7/24"), 0)
	handler.Update(monitoredInterface("192.0.2.7/24", "2001:db8:1::1/64"), 0)
	handler.Update(monitoredInterface("192.0.2.7/24", "2001:db8:1::1/64", "2001:db8:1::2/64"), 0)
	handler.Update(monitoredInterface("192.0.2.7/24", "2001:db8:1::2/64"), 0)
	if resets != 1 {
		t.Fatalf("IPv6 additions or same-prefix privacy retirement reset transports: %d", resets)
	}
	handler.Update(monitoredInterface("192.0.2.7/24"), 0)
	if resets != 2 || flushed != 5 {
		t.Fatalf("loss of newly learned public prefix was missed: resets=%d, refreshes=%d", resets, flushed)
	}
}

func TestDefaultInterfaceChangeHandlerLostAndRecovery(t *testing.T) {
	for _, initialLost := range []bool{false, true} {
		var flushed, resets int
		handler := &defaultInterfaceChangeHandler{
			flushCache:       func() { flushed++ },
			resetConnections: func() { resets++ },
		}
		current := monitoredInterface("192.0.2.7/24")
		if initialLost {
			handler.Update(nil, 0)
			handler.Update(nil, 0)
		} else {
			handler.Update(current, 0)
			handler.Update(current, 0)
			handler.Update(nil, 0)
			handler.Update(nil, 0)
		}
		wantResets := 1
		if !initialLost {
			wantResets++
		}
		if resets != wantResets {
			t.Fatalf("initialLost=%t: repeated loss reset transports: %d", initialLost, resets)
		}
		handler.Update(current, 0)
		handler.Update(current, 0)
		if resets != wantResets+1 || flushed != 2*(wantResets+1) {
			t.Fatalf("initialLost=%t: recovery must reset once: resets=%d, refreshes=%d", initialLost, resets, flushed)
		}
	}
}

func TestDefaultInterfaceChangeHandlerOwnsLastValidSnapshot(t *testing.T) {
	for name, change := range map[string]func(*control.Interface){
		"identity":      func(i *control.Interface) { i.Index++ },
		"hardware":      func(i *control.Interface) { i.HardwareAddr[0]++ },
		"IPv4 source":   func(i *control.Interface) { i.Addresses[0] = netip.MustParsePrefix("192.0.2.8/24") },
		"public prefix": func(i *control.Interface) { i.Addresses[1] = netip.MustParsePrefix("2001:db8:2::1/64") },
	} {
		t.Run(name, func(t *testing.T) {
			var resets int
			handler := &defaultInterfaceChangeHandler{
				flushCache:       func() {},
				resetConnections: func() { resets++ },
			}
			current := monitoredInterface("192.0.2.7/24", "2001:db8:1::1/64")
			handler.Update(current, 0)
			change(current) // The caller may reuse both its object and its slices.
			handler.Update(current, 0)
			if resets != 2 {
				t.Fatalf("mutating caller-owned data also changed remembered snapshot: %d resets", resets)
			}
		})
	}

	var flushed, resets int
	handler := &defaultInterfaceChangeHandler{
		flushCache:       func() { flushed++ },
		resetConnections: func() { resets++ },
	}
	handler.Update(monitoredInterface("192.0.2.7/24", "2001:db8:1::1/64"), 0)
	incomplete := monitoredInterface("192.0.2.7/24")
	incomplete.Addresses = append(incomplete.Addresses, netip.Prefix{})
	handler.Update(incomplete, 0)
	if resets != 1 || flushed != 2 {
		t.Fatalf("incomplete snapshot invalidated connections: resets=%d, refreshes=%d", resets, flushed)
	}
	handler.Update(monitoredInterface("192.0.2.7/24"), 0)
	if resets != 2 {
		t.Fatal("incomplete snapshot replaced last valid public prefix")
	}
}

func TestListenerCloseRejectsLateDefaultInterfaceCallbacks(t *testing.T) {
	var flushed, resets int
	handler := &defaultInterfaceChangeHandler{
		flushCache:       func() { flushed++ },
		resetConnections: func() { resets++ },
	}
	handler.Update(monitoredInterface("192.0.2.7/24"), 0)
	listener := &Listener{interfaceChangeHandler: handler}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	handler.Close() // Closing the barrier is idempotent.
	handler.Update(nil, 0)
	handler.Update(monitoredInterface("198.51.100.7/24"), 1)
	if resets != 1 || flushed != 1 {
		t.Fatalf("callback queued before monitor close still had side effects: resets=%d, refreshes=%d", resets, flushed)
	}
	var absent *defaultInterfaceChangeHandler
	absent.Close() // A listener may fail construction before the monitor exists.
}

func TestDefaultInterfaceCloseWaitsForCallbackSideEffects(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var resets int
	handler := &defaultInterfaceChangeHandler{
		flushCache: func() {},
		resetConnections: func() {
			close(entered)
			<-release
			resets++
		},
	}
	updated := make(chan struct{})
	go func() {
		handler.Update(monitoredInterface("192.0.2.7/24"), 0)
		close(updated)
	}()
	<-entered
	closing := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		close(closing)
		handler.Close()
		close(closed)
	}()
	<-closing
	select {
	case <-closed:
		close(release)
		t.Fatal("Close returned before the callback's reset invocation completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	for _, done := range []chan struct{}{updated, closed} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("callback and Close failed to finish")
		}
	}
	handler.Update(nil, 0)
	if resets != 1 {
		t.Fatalf("a late callback escaped the shutdown barrier: %d resets", resets)
	}
}

func TestDefaultInterfaceConcurrentUpdatesAndClose(t *testing.T) {
	var closed atomic.Bool
	var lateSideEffects atomic.Int32
	sideEffect := func() {
		if closed.Load() {
			lateSideEffects.Add(1)
		}
	}
	handler := &defaultInterfaceChangeHandler{flushCache: sideEffect, resetConnections: sideEffect}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for event := 0; event < 32; event++ {
				handler.Update(monitoredInterface("192.0.2.7/24", "2001:db8:1::1/64"), 0)
				handler.Update(nil, 0)
			}
		}()
	}
	close(start)
	handler.Close()
	closed.Store(true)
	workers.Wait()
	handler.Update(monitoredInterface("198.51.100.7/24"), 1)
	if count := lateSideEffects.Load(); count != 0 {
		t.Fatalf("%d callback side effects started after Close returned", count)
	}
}

package tunnel_test

import (
	"context"
	"testing"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/component/dialer"
	"github.com/metacubex/mihomo/component/dnsstats"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/tunnel"
)

func dnsStatsTestPlan(t *testing.T, proxy C.ProxyAdapter) *tunnel.DNSRoutingPlan {
	t.Helper()
	ctx := icontext.WithDNSFixedOutbound(context.Background(), proxy)
	plan, err := tunnel.PrepareDNSRouting(ctx, "stats.example", &C.Metadata{Type: C.INNER, NetWork: C.UDP})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func observeTestOutbound(proxy C.ProxyAdapter) dnsstats.Outbound {
	return tunnel.NewDNSDialer(nil, proxy, "").ObservabilityOutbound()
}

func TestDNSObservedOutboundAggregatesReloadsWithoutReusingNativeState(t *testing.T) {
	m := dnsstats.New()
	ctx := dnsstats.WithManager(context.Background(), m)
	const endpoint = "https://dns.example/dns-query"
	var first *tunnel.DNSRoutingPlan
	var firstProxy C.Proxy
	var rowID, instanceID string
	var attempts []*dnsstats.Attempt
	for reload := 0; reload < 3; reload++ {
		// ParseProxy is the common construction path for static and provider
		// nodes. Every iteration builds a new real adapter, as a full reload does.
		proxy, err := adapter.ParseProxy(map[string]any{
			"type": "socks5", "name": "same-name", "server": "192.0.2.10",
			"port": 1080, "username": "test-user", "password": "test-secret", "udp": true,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = proxy.Close() })
		plan := dnsStatsTestPlan(t, proxy)
		observed := plan.ObservabilityOutbound()
		if observed.Key == "" || observed.Name != "same-name" || observed.Type != "proxy" {
			t.Fatalf("parsed physical leaf was not observed: %+v", observed)
		}
		if first == nil {
			first = plan
			firstProxy = proxy
		} else {
			if first.ObservabilityOutbound() != observed {
				t.Fatal("same physical configuration split statistics after reconstruction")
			}
			if first.NativeTransportKey() == plan.NativeTransportKey() || first.TransportKey() == plan.TransportKey() || first.CacheKey() == plan.CacheKey() {
				t.Fatal("stable statistics identity weakened transport or cache replacement boundaries")
			}
		}
		dnsDialer := tunnel.NewDNSDialer(nil, proxy.Adapter(), "")
		if dnsDialer.ObservabilityOutbound() != observed || dnsDialer.WithPlan(plan, ctx).ObservabilityOutbound() != observed {
			t.Fatal("explicit and frozen transport observations disagree on the same leaf")
		}
		m.Configure(true) // ordinary reload keeps process observation enabled
		attempts = append(attempts, dnsstats.StartUpstream(ctx, endpoint, observed))
		rows := m.Upstreams()
		if len(rows.Items) != 1 || rows.Items[0].Attempts != uint64(reload+1) {
			t.Fatalf("reload created duplicate displayed upstream rows: %+v", rows)
		}
		if reload == 0 {
			rowID, instanceID = rows.Items[0].ID, rows.InstanceID
		} else if rows.Items[0].ID != rowID || rows.InstanceID != instanceID {
			t.Fatal("normal reload changed the row or observation instance identity")
		}
	}
	// An older adapter's in-flight exchange may complete after reload. Its
	// completion must update the same row as the newly constructed adapter.
	for _, attempt := range attempts {
		attempt.Complete(dnsstats.UpstreamSuccess)
	}
	if row := m.Upstreams().Items[0]; row.Successes != 3 || row.Attempts != 3 {
		t.Fatalf("reload lost or duplicated exchange completions: %+v", row)
	}
	oldRouting := tunnel.DNSRuleRoutingEnabled()
	t.Cleanup(func() { tunnel.SetDNSRuleRouting(oldRouting) })
	tunnel.SetDNSRuleRouting(!oldRouting)
	otherEpoch := dnsStatsTestPlan(t, firstProxy)
	if otherEpoch.ObservabilityOutbound() != first.ObservabilityOutbound() || otherEpoch.NativeTransportKey() == first.NativeTransportKey() {
		t.Fatal("routing epoch must isolate transports while preserving same-exit statistics")
	}
}

func TestDNSObservedOutboundPreservesRealExitAndUnknownBoundaries(t *testing.T) {
	m := dnsstats.New()
	ctx := dnsstats.WithManager(context.Background(), m)
	const endpoint = "udp://192.0.2.53:53"
	for _, password := range []string{"first-secret", "second-secret", "first-secret"} {
		proxy, err := adapter.ParseProxy(map[string]any{
			"type": "socks5", "name": "same-name", "server": "192.0.2.10",
			"port": 1080, "password": password, "udp": true,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = proxy.Close() })
		dnsstats.StartUpstream(ctx, endpoint, observeTestOutbound(proxy)).Complete(dnsstats.UpstreamSuccess)
	}
	for i := 0; i < 2; i++ {
		dnsstats.StartUpstream(ctx, endpoint, observeTestOutbound(adapter.NewProxy(outbound.NewDirect()))).Complete(dnsstats.UpstreamSuccess)
	}
	dnsstats.StartUpstream(ctx, endpoint, observeTestOutbound(nil)).Complete(dnsstats.UpstreamSuccess)
	rows := m.Upstreams().Items
	if len(rows) != 4 {
		t.Fatalf("distinct credentials, DIRECT and unknown need separate rows: %+v", rows)
	}
	var proxyRows, directRows, unknownRows int
	for _, row := range rows {
		switch row.OutboundType {
		case "proxy":
			proxyRows++
		case "direct":
			directRows++
			if row.Outbound != "DIRECT" || row.Attempts != 2 {
				t.Fatalf("reconstructed built-in DIRECT split statistics: %+v", row)
			}
		case "unknown":
			unknownRows++
			if row.Outbound != "" || row.Attempts != 1 {
				t.Fatalf("unknown exit was guessed or merged: %+v", row)
			}
		}
	}
	if proxyRows != 2 || directRows != 1 || unknownRows != 1 {
		t.Fatal("outbound type boundaries changed")
	}
	for _, kind := range []C.AdapterType{C.Selector, C.Relay, C.Reject} {
		if got := observeTestOutbound(outbound.NewBase(outbound.BaseOption{Name: "opaque", Type: kind, UDP: true})); got.Key != "" {
			t.Fatal("opaque selection or rejection acquired a physical exit identity")
		}
	}
	opaqueDialer, err := adapter.ParseProxy(map[string]any{
		"type": "socks5", "name": "opaque", "server": "192.0.2.10", "port": 1080,
		"dialer-proxy": "dynamic", "udp": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opaqueDialer.Close() })
	if observeTestOutbound(opaqueDialer).Key != "" {
		t.Fatal("unfrozen dialer-proxy acquired a physical exit identity")
	}
	first := outbound.NewBase(outbound.BaseOption{Name: "external", Type: C.Socks5, UDP: true})
	second := outbound.NewBase(outbound.BaseOption{Name: "external", Type: C.Socks5, UDP: true})
	if observeTestOutbound(first).Key == observeTestOutbound(second).Key {
		t.Fatal("external adapters without configuration identities were merged by name")
	}
}

func TestDNSObservedOutboundKeepsEffectiveSocketPolicy(t *testing.T) {
	oldInterface, oldMark := dialer.DefaultInterface.Load(), dialer.DefaultRoutingMark.Load()
	t.Cleanup(func() { dialer.DefaultInterface.Store(oldInterface); dialer.DefaultRoutingMark.Store(oldMark) })
	dialer.DefaultInterface.Store("stats-first")
	dialer.DefaultRoutingMark.Store(10)
	proxy := outbound.NewDirect()
	first := observeTestOutbound(proxy)
	dialer.DefaultInterface.Store("stats-second")
	if observeTestOutbound(proxy) == first {
		t.Fatal("changed effective interface was merged into the previous exit")
	}
	dialer.DefaultInterface.Store("stats-first")
	dialer.DefaultRoutingMark.Store(20)
	if observeTestOutbound(proxy) == first {
		t.Fatal("changed effective routing mark was merged into the previous exit")
	}
	explicit := outbound.NewDirectWithOption(outbound.DirectOption{Name: "bound", BasicOption: outbound.BasicOption{Interface: "explicit", RoutingMark: 30}})
	bound := observeTestOutbound(explicit)
	dialer.DefaultInterface.Store("stats-third")
	dialer.DefaultRoutingMark.Store(40)
	if observeTestOutbound(explicit) != bound {
		t.Fatal("unrelated defaults split an explicitly bound outbound")
	}
}

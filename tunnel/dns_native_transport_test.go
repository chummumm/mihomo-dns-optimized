package tunnel

import (
	"context"
	"reflect"
	"testing"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/tunnel/statistic"
)

func TestDNSRoutingNativeQueryTracksCurrentOriginAndCancelsOnlyItself(t *testing.T) {
	leaf := newDNSProxyTestBase("leaf", C.Socks5, true)
	leaf.provider = "nodes"
	group := newDNSProxyTestBase("group", C.Selector, true)
	group.provider = "groups"
	origin := dnsProxyTestResolver(C.UDP)
	origin.Process, origin.ProcessPath = "resolver", "/usr/bin/resolver"
	rule := dnsProxyTestRule(t, "DOMAIN,first.example,leaf")
	plan := &DNSRoutingPlan{origin: origin, inbound: true, route: dnsProxyRoute{proxy: leaf, qname: "first.example", rule: rule, groups: []C.ProxyAdapter{group}}}
	before := origin.Clone()
	up, down := statistic.DefaultManager.Total()
	ctx1, first, err := plan.TrackNativeQuery(context.Background(), "192.0.2.53:443", C.TCP, 31)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	secondPlan := *plan
	secondPlan.origin = origin.Clone()
	secondPlan.origin.SrcPort++
	secondPlan.route.qname = "second.example"
	secondPlan.route.rule = dnsProxyTestRule(t, "DOMAIN,second.example,leaf")
	ctx2, second, err := secondPlan.TrackNativeQuery(context.Background(), "192.0.2.53:443", C.TCP, 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	first.DownloadTotal.Add(63)
	if !reflect.DeepEqual(origin, before) {
		t.Fatal("query display metadata modified the original flow")
	}
	if plan.TransportKey() != secondPlan.TransportKey() {
		t.Fatal("QNAME/source-port changes unnecessarily split the transport pool")
	}
	secondPlan.epoch++
	if plan.TransportKey() == secondPlan.TransportKey() {
		t.Fatal("runtime epoch changes reused an obsolete transport scope")
	}
	for _, tracker := range []*DNSNativeQueryTracker{first, second} {
		md := tracker.Metadata
		if md.DstIP.String() != "192.0.2.53" || md.DstPort != 443 || md.NetWork != C.UDP || md.InName != origin.InName || md.Process != origin.Process || md.ProcessPath != origin.ProcessPath || md.InUser != origin.InUser || md.SrcIP != origin.SrcIP {
			t.Fatalf("query origin or actual endpoint was lost: %+v", md)
		}
		if !reflect.DeepEqual(tracker.Chains(), C.Chain{"leaf", "group"}) || !reflect.DeepEqual(tracker.ProviderChains(), C.Chain{"nodes", "groups"}) {
			t.Fatalf("query route chains are incomplete: %+v", tracker.Info())
		}
		if statistic.DefaultManager.Get(tracker.ID()) != tracker {
			t.Fatal("active query missing from the existing connections API")
		}
	}
	if first.Metadata.Host != "first.example" || first.RulePayload != "first.example" || second.Metadata.Host != "second.example" || second.RulePayload != "second.example" || first.Metadata.SrcPort == second.Metadata.SrcPort {
		t.Fatal("shared transport labelled later queries with the first query's metadata")
	}
	if first.UploadTotal.Load() != 31 || first.DownloadTotal.Load() != 63 {
		t.Fatal("logical DNS payload counters are incorrect")
	}
	if gotUp, gotDown := statistic.DefaultManager.Total(); gotUp != up || gotDown != down {
		t.Fatal("logical query payload was double-counted in encrypted wire totals")
	}
	_ = statistic.DefaultManager.Get(first.ID()).Close()
	if ctx1.Err() == nil || ctx2.Err() != nil || statistic.DefaultManager.Get(first.ID()) != nil || statistic.DefaultManager.Get(second.ID()) == nil {
		t.Fatal("closing one logical query must leave its multiplexed sibling alive")
	}
	_ = second.Close()
	if ctx2.Err() == nil || statistic.DefaultManager.Get(second.ID()) != nil {
		t.Fatal("completed query retained a dashboard entry")
	}
	internalPlan := *plan
	internalPlan.inbound = false
	_, internal, err := internalPlan.TrackNativeQuery(context.Background(), "192.0.2.53:443", C.TCP, 33)
	if err != nil {
		t.Fatal(err)
	}
	defer internal.Close()
	if internal.Metadata.NetWork != C.TCP {
		t.Fatal("an internal lookup without a DNS client must display its actual upstream protocol")
	}
}

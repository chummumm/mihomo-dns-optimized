package dns

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/dnsstats"
	icontext "github.com/metacubex/mihomo/context"
	D "github.com/miekg/dns"
)

func TestDNSObservabilityBootstrapDoesNotHideBusinessAtSameEndpoint(t *testing.T) {
	m := dnsstats.New()
	ctx := observabilityTestContext(m)
	endpoint := "udp://192.0.2.53:53"
	for _, name := range []string{"node.example", "doh.example"} {
		query := routingTestQuery(name, 1)
		finishDNSUpstream(observeDNSUpstream(icontext.WithDNSBootstrap(ctx), endpoint), new(D.Msg).SetReply(query), nil)
	}
	if len(m.Upstreams().Items) != 0 {
		t.Fatal("infrastructure lookups created visible upstream rows")
	}
	finishDNSUpstream(observeDNSUpstream(ctx, endpoint), &D.Msg{}, nil)
	rows := m.Upstreams().Items
	if len(rows) != 1 || rows[0].Attempts != 1 {
		t.Fatalf("endpoint-wide filtering hid ordinary DNS work: %+v", rows)
	}
}

func TestDNSObservabilityFastestIPContributingSources(t *testing.T) {
	query := routingTestQuery("provenance.example", 1)
	m := dnsstats.New()
	ctx := withDNSAnswerSources(withDNSAnswerLifetimes(observabilityTestContext(m)))
	checker := speedCheckTestChecker(t, time.Second, 4)
	checker.probe = func(_ context.Context, ip netip.Addr, _ speedCheckMode) (time.Duration, error) {
		if ip == netip.MustParseAddr("192.0.2.9") {
			return 200 * time.Millisecond, nil
		}
		return time.Millisecond, nil
	}
	var clients []dnsClient
	for i, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.9"} {
		outbound := newRoutingTestOutbound([]string{"direct-a", "direct-b", "slow"}[i])
		answer := selectionTestAnswer(query, "", ip)
		outbound.answer = func(q *D.Msg) *D.Msg { reply := answer.Copy(); reply.Id = q.Id; return reply }
		client := newClient(netip.AddrPortFrom(netip.MustParseAddr(ip), 53).String(), nil, "udp", nil, outbound, "")
		t.Cleanup(func() { client.Close() })
		clients = append(clients, client)
	}
	answer, _, err := checker.Exchange(ctx, clients, query)
	if err != nil {
		t.Fatal(err)
	}
	sources := dnsAnswerSourcesFor(ctx, answer)
	if len(msgToIP(answer)) != 2 || len(sources) != 2 || sources[0].Outbound != "direct-a" || sources[1].Outbound != "direct-b" {
		t.Fatalf("final answer attributed to unused or missing sources: %v, %+v", answer, sources)
	}
}

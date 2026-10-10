package dns

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/dnsstats"
	"github.com/metacubex/mihomo/component/fakeip"
	"github.com/metacubex/mihomo/component/resolver"
	"github.com/metacubex/mihomo/component/trie"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"

	D "github.com/miekg/dns"
)

func observabilityTestContext(manager *dnsstats.Manager) context.Context {
	ctx := dnsstats.WithSource(context.Background(), dnsstats.Source{Client: "192.0.2.10", Protocol: "udp", Name: "DNS"})
	return dnsstats.WithManager(ctx, manager)
}

func observabilityTestService(t *testing.T, outbound *routingTestOutbound) (*Resolver, *Service) {
	t.Helper()
	client := newClient("192.0.2.53:53", nil, "udp", nil, outbound, "")
	r := NewResolverFromClient(client)
	t.Cleanup(r.Close)
	return r, NewService(r, NewEnhancer(EnhancerConfig{IPv6: true, EnhancedMode: C.DNSNormal}))
}

func TestDNSObservabilityCacheAndInternalQueryBoundaries(t *testing.T) {
	m := dnsstats.New()
	ctx := observabilityTestContext(m)
	outbound := newRoutingTestOutbound("observed")
	r, service := observabilityTestService(t, outbound)
	query := routingTestQuery("cache-observation.example", 12)
	query.SetEdns0(1232, true)
	for i := 0; i < 2; i++ {
		answer, err := service.ServeMsg(ctx, query)
		if err != nil || answer == nil || answer.Id != query.Id || answer.IsEdns0() == nil || !answer.IsEdns0().Do() {
			t.Fatalf("observation changed DNS answer: %v, %v", answer, err)
		}
	}
	stats := m.Stats()
	if stats.Totals.Queries != 2 || stats.Totals.Upstream != 1 || stats.Totals.CacheFresh != 1 || outbound.udp.Load() != 1 {
		t.Fatalf("bad client/cache accounting: %+v, calls=%d", stats.Totals, outbound.udp.Load())
	}
	upstreams := m.Upstreams()
	if len(upstreams.Items) != 1 || upstreams.Items[0].Attempts != 1 || upstreams.Items[0].Successes != 1 {
		t.Fatalf("cache hit counted as upstream exchange: %+v", upstreams)
	}
	page := m.Queries(dnsstats.Filter{})
	for _, record := range page.Items {
		if record.QName != "cache-observation.example" || record.Client != "192.0.2.10" || record.Protocol != "udp" || record.Source != "DNS" || record.RCode != "NOERROR" || len(record.Answers) == 0 {
			t.Fatalf("lost query summary: %+v", record)
		}
	}
	// Force a still-retained cache entry into its stale window. Its refresh is
	// real upstream work, but must not create another client query event.
	key := query.Question[0].String()
	cached, _, ok := r.cache.GetWithExpire(key)
	if !ok {
		t.Fatal("answer was not cached")
	}
	r.cache.SetWithExpire(key, cached, time.Now().Add(-time.Second))
	if _, err := service.ServeMsg(ctx, query); err != nil {
		t.Fatal(err)
	}
	if stats := m.Stats(); stats.Totals.Queries != 3 || stats.Totals.CacheStale != 1 {
		t.Fatalf("stale reply misclassified: %+v", stats.Totals)
	}
	internal := routingTestQuery("bootstrap-observation.example", 13)
	if _, err := r.ExchangeContext(icontext.WithDNSBootstrap(ctx), internal); err != nil {
		t.Fatal(err)
	}
	if m.Stats().Totals.Queries != 3 {
		t.Fatal("internal resolver call counted as a client query")
	}
	// Explicit internal service invocations are excluded defensively too.
	if _, err := service.ServeMsg(icontext.WithDNSBootstrap(ctx), internal); err != nil {
		t.Fatal(err)
	}
	if m.Stats().Totals.Queries != 3 {
		t.Fatal("bootstrap service call counted as a client query")
	}
}

type observabilityJoinedContext struct {
	context.Context
	joined chan<- struct{}
	once   sync.Once
}

func (c *observabilityJoinedContext) Done() <-chan struct{} {
	// In this non-routing foreground path the original request's Done is
	// first read by exchangeWithoutCache's select, after DoChan has joined
	// the group. Shared work uses WithoutCancel and cannot signal this hook.
	c.once.Do(func() { c.joined <- struct{}{} })
	return c.Context.Done()
}

func TestDNSObservabilitySharedQueriesHaveSeparateClientEvents(t *testing.T) {
	m := dnsstats.New()
	outbound := newRoutingTestOutbound("shared-observation")
	gate := make(chan struct{})
	outbound.gate = gate
	_, service := observabilityTestService(t, outbound)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	t.Cleanup(release)
	const clients = 16
	var group sync.WaitGroup
	errors := make(chan error, clients)
	joined := make(chan struct{}, clients)
	for i := 0; i < clients; i++ {
		group.Add(1)
		go func(id int) {
			defer group.Done()
			ctx := &observabilityJoinedContext{Context: observabilityTestContext(m), joined: joined}
			_, err := service.ServeMsg(ctx, routingTestQuery("shared-observation.example", uint16(id+1)))
			errors <- err
		}(i)
	}
	select {
	case <-outbound.questions:
	case <-time.After(3 * time.Second):
		t.Fatal("first query never reached upstream")
	}
	// Merely starting goroutines does not prove that they joined the flight:
	// a caller may pause between its cache miss and DoChan. Keep the actual
	// upstream blocked until every client has reached the shared wait.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for i := 0; i < clients; i++ {
		select {
		case <-joined:
		case <-deadline.C:
			t.Fatalf("only %d of %d clients joined the shared flight", i, clients)
		}
	}
	release()
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if m.Stats().Totals.Queries != clients || m.Status().Retained != clients {
		t.Fatal("singleflight collapsed client query events")
	}
	if got := m.Upstreams(); len(got.Items) != 1 || got.Items[0].Attempts != 1 {
		t.Fatalf("shared/answer cache work duplicated upstream count: %+v", got)
	}
}

func TestDNSObservabilityCandidateCacheDoesNotCountNetwork(t *testing.T) {
	ctx, checker := candidateCacheTestScope(t)
	m := dnsstats.New()
	ctx = dnsstats.WithManager(ctx, m)
	client := newClient("192.0.2.53:53", nil, "udp", nil, nil, "")
	query := routingTestQuery("cache.example", 101)
	for i := 0; i < 2; i++ {
		query.Id++
		candidateCacheTestExchange(t, ctx, checker, []dnsClient{client}, query)
	}
	upstreams := m.Upstreams()
	if len(upstreams.Items) != 1 || upstreams.Items[0].Attempts != 1 {
		t.Fatalf("candidate reuse counted as another exchange: %+v", upstreams)
	}
	if m.Stats().Totals.Queries != 0 {
		t.Fatal("auxiliary candidate work counted as a client query")
	}
}

func TestDNSObservabilityRejectDropAndErrorAreDistinct(t *testing.T) {
	routingTestEnable(t)
	outbound := newRoutingTestOutbound("observability-policy")
	r := routingTestResolver()
	t.Cleanup(r.Close)
	service := NewService(r, NewEnhancer(EnhancerConfig{IPv6: true}))
	m := dnsstats.New()
	ctx := dnsstats.WithManager(routingTestContext(outbound, routingTestOrigin()), m)
	query := routingTestQuery("blocked-observation.example", 32)
	outbound.kind.Store(int32(C.Reject))
	answer, err := service.ServeMsg(ctx, query)
	if err != nil || answer.Rcode != D.RcodeRefused {
		t.Fatalf("REJECT semantics changed: %v, %v", answer, err)
	}
	outbound.kind.Store(int32(C.RejectDrop))
	if _, err = service.ServeMsg(ctx, query); !errors.Is(err, resolver.ErrDNSDrop) {
		t.Fatalf("DROP semantics changed: %v", err)
	}
	failed := &Service{handler: func(_ *icontext.DNSContext, _ *D.Msg) (*D.Msg, error) {
		return nil, errors.New("secret URL https://user:password@dns.example/token")
	}}
	if _, err = failed.ServeMsg(ctx, query); err == nil {
		t.Fatal("unexpected success")
	}
	stats := m.Stats().Totals
	if stats.Queries != 3 || stats.Reject != 1 || stats.Drop != 1 || stats.Errors != 1 || len(m.Upstreams().Items) != 0 {
		t.Fatalf("bad outcome counts: %+v", stats)
	}
	page := m.Queries(dnsstats.Filter{})
	if page.Items[0].Error != "resolution_failed" || page.Items[0].RCode != "SERVFAIL" || page.Items[1].RCode != "" || page.Items[1].Outcome != "drop" {
		t.Fatalf("error/drop result leaked data or fabricated response: %+v", page.Items)
	}
}

func TestDNSObservabilityHostsAliasAndFakeIP(t *testing.T) {
	oldHosts := resolver.DefaultHosts
	t.Cleanup(func() { resolver.DefaultHosts = oldHosts })
	hosts := trie.New[resolver.HostValue]()
	if err := hosts.Insert("hosts.example", resolver.HostValue{IPs: []netip.Addr{netip.MustParseAddr("192.0.2.7")}}); err != nil {
		t.Fatal(err)
	}
	if err := hosts.Insert("alias.example", resolver.HostValue{IsDomain: true, Domain: "target.example"}); err != nil {
		t.Fatal(err)
	}
	resolver.DefaultHosts = resolver.NewHosts(hosts)
	m := dnsstats.New()
	ctx := observabilityTestContext(m)
	r := NewResolverFromClient(&redirHostTestClient{})
	t.Cleanup(r.Close)
	service := NewService(r, NewEnhancer(EnhancerConfig{IPv6: true, UseHosts: true}))
	for _, name := range []string{"hosts.example", "Alias.Example"} {
		if _, err := service.ServeMsg(ctx, routingTestQuery(name, 17)); err != nil {
			t.Fatal(err)
		}
	}
	page := m.Queries(dnsstats.Filter{})
	if page.Items[0].QName != "alias.example" || page.Items[1].Outcome != "hosts" || m.Stats().Totals.Hosts != 1 {
		t.Fatalf("alias lost original query or hosts miscounted: %+v", page.Items)
	}
	pool, err := fakeip.New(fakeip.Options{IPNet: netip.MustParsePrefix("198.18.0.1/16"), Size: 16})
	if err != nil {
		t.Fatal(err)
	}
	service = NewService(nil, NewEnhancer(EnhancerConfig{EnhancedMode: C.DNSFakeIP, FakeIPPool: pool, FakeIPSkipper: &fakeip.Skipper{}, FakeIPTTL: 17}))
	if _, err := service.ServeMsg(ctx, routingTestQuery("fake.example", 18)); err != nil {
		t.Fatal(err)
	}
	if m.Stats().Totals.FakeIP != 1 || m.Stats().Totals.CacheFresh != 0 {
		t.Fatal("fake IP counted as DNS answer cache hit")
	}
}

func TestDNSObservabilityBoundedAnswersAndNXDOMAIN(t *testing.T) {
	message := new(D.Msg)
	message.Answer = []D.RR{&D.TXT{Hdr: D.RR_Header{Rrtype: D.TypeTXT, Ttl: 50}, Txt: []string{strings.Repeat("x", 5000)}}}
	answers, truncated := summarizeDNSAnswers(message.Answer)
	if !truncated || len(answers) != 1 || len(answers[0].Value) > dnsstats.MaxAnswerBytes {
		t.Fatal("TXT summary was not bounded")
	}
	m := dnsstats.New()
	ctx := observabilityTestContext(m)
	for _, rcode := range []int{D.RcodeSuccess, D.RcodeNameError, D.RcodeServerFailure, D.RcodeRefused} {
		attempt := observeDNSUpstream(ctx, "udp://192.0.2.53:53")
		finishDNSUpstream(attempt, &D.Msg{MsgHdr: D.MsgHdr{Rcode: rcode}}, nil)
	}
	finishDNSUpstream(observeDNSUpstream(ctx, "udp://192.0.2.53:53"), nil, context.Canceled)
	finishDNSUpstream(observeDNSUpstream(ctx, "udp://192.0.2.53:53"), nil, context.DeadlineExceeded)
	stats := m.Upstreams().Items[0]
	if stats.Attempts != 6 || stats.Successes != 2 || stats.Errors != 3 || stats.Canceled != 1 || stats.Timeouts != 1 || stats.RCodeErrors != 2 {
		t.Fatalf("NXDOMAIN or cancellation misclassified: %+v", stats)
	}
}

func TestDNSObservabilityLocalRCodeProvenance(t *testing.T) {
	for _, qtype := range []uint16{D.TypeA, D.TypeTXT} {
		for _, test := range []struct {
			name, outcome string
			rcode         int
		}{{"success", "local", D.RcodeSuccess}, {"name_error", "local", D.RcodeNameError}, {"refused", "reject", D.RcodeRefused}, {"server_failure", "error", D.RcodeServerFailure}} {
			t.Run(fmt.Sprintf("%d/%s", qtype, test.name), func(t *testing.T) {
				m := dnsstats.New()
				r := NewResolverFromClient(newRCodeClient(test.name))
				// Exercise provenance through a successful answer policy copy too.
				r.answerPolicy = AnswerPolicy{RRTTLMin: 1}
				t.Cleanup(r.Close)
				service := NewService(r, NewEnhancer(EnhancerConfig{IPv6: true}))
				query := new(D.Msg).SetQuestion("local-rcode.example.", qtype)
				answer, err := service.ServeMsg(observabilityTestContext(m), query)
				if err != nil || answer.Rcode != test.rcode {
					t.Fatalf("RCODE behavior changed: %v, %v", answer, err)
				}
				page := m.Queries(dnsstats.Filter{})
				if len(page.Items) != 1 || page.Items[0].Outcome != test.outcome || page.Items[0].Cache != "none" {
					t.Fatalf("local RCODE mislabeled: %+v", page.Items)
				}
				if len(m.Upstreams().Items) != 0 || m.Stats().Totals.Upstream != 0 {
					t.Fatal("local RCODE counted as upstream exchange")
				}
			})
		}
	}
}

func TestDNSObservabilityCachedErrorsKeepCacheHitAndErrorCounts(t *testing.T) {
	for _, rcode := range []int{D.RcodeServerFailure, D.RcodeRefused} {
		t.Run(dnsResponseCode(rcode), func(t *testing.T) {
			m := dnsstats.New()
			r := NewResolverFromClient(newRCodeClient("success"))
			t.Cleanup(r.Close)
			service := NewService(r, NewEnhancer(EnhancerConfig{IPv6: true}))
			query := routingTestQuery("cached-error.example", 55)
			cached := new(D.Msg).SetRcode(query, rcode)
			r.cache.SetWithExpire(query.Question[0].String(), cached, time.Now().Add(time.Minute))
			answer, err := service.ServeMsg(observabilityTestContext(m), query)
			if err != nil || answer.Rcode != rcode {
				t.Fatalf("cached RCODE changed: %v, %v", answer, err)
			}
			stats := m.Stats()
			if stats.Totals.Queries != 1 || stats.Totals.CacheFresh != 1 || stats.Totals.Errors != 1 || stats.Last24H.Errors != 1 {
				t.Fatalf("cache and outcome not independent: %+v", stats.Totals)
			}
			page := m.Queries(dnsstats.Filter{Outcome: "error"})
			if len(page.Items) != 1 || page.Items[0].Cache != "fresh" || page.Items[0].Error == "" {
				t.Fatalf("error filter lost cached failure: %+v", page.Items)
			}
			if len(m.Upstreams().Items) != 0 {
				t.Fatal("cached error contacted upstream")
			}
		})
	}
}

// Let all other replies acquire provenance before allowing the eventual
// winner to be synthesized. Losers finish once the picker selects that winner.
type observabilityLocalBarrierClient struct {
	dnsClient
	marked *sync.WaitGroup
	winner bool
}

func (c observabilityLocalBarrierClient) ExchangeContext(ctx context.Context, query *D.Msg) (*D.Msg, error) {
	if c.winner {
		c.marked.Wait()
	}
	message, err := c.dnsClient.ExchangeContext(ctx, query)
	if !c.winner {
		c.marked.Done()
		<-ctx.Done()
	}
	return message, err
}

func TestDNSObservabilityLocalProvenanceFanout(t *testing.T) {
	for _, test := range []struct {
		name       string
		clients    int
		policyCopy bool
		lateWinner bool
	}{
		{"32-with-policy-copy", 32, true, false},
		{"64-with-late-winner", 64, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := dnsstats.New()
			clients := make([]dnsClient, test.clients)
			var marked sync.WaitGroup
			if test.lateWinner {
				marked.Add(test.clients - 1)
			}
			for i := range clients {
				clients[i] = clientWithDisableTypes{
					dnsClient:    newRCodeClient("success"),
					disableTypes: map[uint16]struct{}{D.TypeA: {}},
				}
				if test.lateWinner {
					clients[i] = observabilityLocalBarrierClient{dnsClient: clients[i], marked: &marked, winner: i == len(clients)-1}
				}
			}
			r := NewResolverFromClient(clients[0])
			r.main = clients
			if test.policyCopy {
				r.answerPolicy = AnswerPolicy{RRTTLMin: 1}
			}
			t.Cleanup(r.Close)
			service := NewService(r, NewEnhancer(EnhancerConfig{IPv6: true}))
			query := routingTestQuery("local-fanout.example", 89)
			answer, err := service.ServeMsg(observabilityTestContext(manager), query)
			if err != nil || answer == nil || answer.Rcode != D.RcodeSuccess || len(answer.Answer) != 0 || answer.Id != query.Id {
				t.Fatalf("local empty answer changed: %v, %v", answer, err)
			}
			page := manager.Queries(dnsstats.Filter{})
			if len(page.Items) != 1 || page.Items[0].Outcome != "local" || page.Items[0].Cache != "none" {
				t.Fatalf("local provenance lost at fan-out boundary: %+v", page.Items)
			}
			if stats := manager.Stats().Totals; stats.Queries != 1 || stats.Local != 1 || stats.Upstream != 0 || len(manager.Upstreams().Items) != 0 {
				t.Fatalf("local clients counted as upstream work: %+v", stats)
			}
		})
	}
}

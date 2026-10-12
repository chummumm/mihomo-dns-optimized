package dns

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/dnsstats"
	"github.com/metacubex/mihomo/component/resolver"
	"github.com/metacubex/mihomo/component/trie"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"

	D "github.com/miekg/dns"
)

func answerPolicyMessage(t *testing.T, qtype uint16, records ...string) (*D.Msg, *D.Msg) {
	t.Helper()
	query := new(D.Msg)
	query.SetQuestion("www.example.", qtype)
	answer := new(D.Msg)
	answer.SetReply(query)
	for _, text := range records {
		rr, err := D.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		answer.Answer = append(answer.Answer, rr)
	}
	return query, answer
}

func TestDNSOptimizationAnswerPolicyFlattenCompleteChain(t *testing.T) {
	for _, test := range []struct {
		qtype   uint16
		address string
	}{
		{D.TypeA, "192.0.2.1"}, {D.TypeAAAA, "2001:db8::1"},
	} {
		t.Run(D.TypeToString[test.qtype], func(t *testing.T) {
			query, answer := answerPolicyMessage(t, test.qtype,
				"www.example. 120 IN CNAME first.example.",
				"FIRST.example. 45 IN CNAME final.example.",
				"final.example. 900 IN "+D.TypeToString[test.qtype]+" "+test.address)
			before := answer.String()
			answer.Authoritative = true
			result := (AnswerPolicy{ForceNoCNAME: true, RRTTLMax: 3600}).apply(query, answer)
			if len(result.Answer) != 1 || result.Answer[0].Header().Name != query.Question[0].Name ||
				result.Answer[0].Header().Ttl != 45 || result.Answer[0].Header().Rrtype != test.qtype || result.Authoritative {
				t.Fatalf("invalid flattened answer: %s", result)
			}
			answer.Authoritative = false
			if answer.String() != before {
				t.Fatal("policy mutated the shared upstream answer")
			}
		})
	}
}

func TestDNSOptimizationAnswerPolicyLeavesIncompleteAndAmbiguousChains(t *testing.T) {
	for _, records := range [][]string{
		{"www.example. 60 IN CNAME missing.example."},
		{"www.example. 60 IN CNAME alias.example.", "alias.example. 60 IN CNAME www.example.", "alias.example. 60 IN A 192.0.2.1"},
		{"www.example. 60 IN CNAME first.example.", "www.example. 60 IN CNAME second.example.", "first.example. 60 IN A 192.0.2.1"},
		{"www.example. 60 IN CNAME alias.example.", "other.example. 60 IN A 192.0.2.1"},
		{"www.example. 60 IN CNAME alias.example.", "www.example. 60 IN A 192.0.2.1", "alias.example. 60 IN A 192.0.2.2"},
		{"www.example. 60 IN CNAME alias.example.", "alias.example. 60 IN A 192.0.2.1", "example. 60 IN DNAME other.example."},
	} {
		query, answer := answerPolicyMessage(t, D.TypeA, records...)
		result := (AnswerPolicy{ForceNoCNAME: true}).apply(query, answer)
		if result.String() != answer.String() {
			t.Fatalf("rewrote unsafe chain:\n%s\n%s", answer, result)
		}
	}
	query, answer := answerPolicyMessage(t, D.TypeCNAME, "www.example. 60 IN CNAME alias.example.")
	if got := (AnswerPolicy{ForceNoCNAME: true}).apply(query, answer); got.String() != answer.String() {
		t.Fatal("rewrote an explicit CNAME question")
	}
}

func TestDNSOptimizationAnswerPolicyTTLAndEDNS(t *testing.T) {
	query, answer := answerPolicyMessage(t, D.TypeA, "www.example. 7200 IN A 192.0.2.1", "www.example. 0 IN A 192.0.2.2")
	soa, err := D.NewRR("example. 7200 IN SOA ns.example. hostmaster.example. 1 60 60 60 7200")
	if err != nil {
		t.Fatal(err)
	}
	answer.Ns = []D.RR{soa}
	answer.SetEdns0(1232, true)
	opt := answer.IsEdns0().String()
	result := (AnswerPolicy{RRTTLMin: 60, RRTTLMax: 3600}).apply(query, answer)
	if result.Answer[0].Header().Ttl != 3600 || result.Answer[1].Header().Ttl != 0 ||
		result.Ns[0].Header().Ttl != 3600 || result.Ns[0].(*D.SOA).Minttl != 3600 || result.IsEdns0().String() != opt {
		t.Fatalf("TTL policy changed EDNS/no-cache or missed lifetimes: %s", result)
	}
	query, answer = answerPolicyMessage(t, D.TypeA, "www.example. 10 IN A 192.0.2.1")
	if result = (AnswerPolicy{RRTTLMin: 60}).apply(query, answer); result.Answer[0].Header().Ttl != 60 {
		t.Fatal("explicit minimum TTL did not apply")
	}
}

func TestDNSOptimizationAnswerPolicyProtectsDNSSEC(t *testing.T) {
	for _, mode := range []string{"DO", "CD", "AD", "RRSIG", "SIG0", "TSIG", "truncated", "mismatched"} {
		t.Run(mode, func(t *testing.T) {
			query, answer := answerPolicyMessage(t, D.TypeA, "www.example. 60 IN CNAME alias.example.", "alias.example. 7200 IN A 192.0.2.1")
			switch mode {
			case "DO":
				query.SetEdns0(1232, true)
			case "CD":
				query.CheckingDisabled = true
			case "AD":
				answer.AuthenticatedData = true
			case "RRSIG":
				answer.Answer = append(answer.Answer, &D.RRSIG{Hdr: D.RR_Header{Name: "alias.example.", Rrtype: D.TypeRRSIG, Class: D.ClassINET, Ttl: 7200}})
			case "SIG0":
				answer.Extra = append(answer.Extra, &D.SIG{RRSIG: D.RRSIG{Hdr: D.RR_Header{Name: ".", Rrtype: D.TypeSIG, Class: D.ClassANY}}})
			case "TSIG":
				answer.SetTsig("key.example.", D.HmacSHA256, 300, time.Now().Unix())
			case "truncated":
				answer.Truncated = true
			case "mismatched":
				answer.Question[0].Name = "other.example."
			}
			before := answer.String()
			result := (AnswerPolicy{ForceNoCNAME: true, RRTTLMin: 120, RRTTLMax: 3600}).apply(query, answer)
			if result.String() != before {
				t.Fatalf("rewrote protected answer %s", result)
			}
		})
	}
}

type answerPolicyTestClient struct {
	answer *D.Msg
	calls  atomic.Int32
}

func (c *answerPolicyTestClient) ExchangeContext(_ context.Context, query *D.Msg) (*D.Msg, error) {
	c.calls.Add(1)
	answer := c.answer.Copy()
	answer.Id = query.Id
	return answer, nil
}
func (*answerPolicyTestClient) Address() string  { return "answer-policy-test" }
func (*answerPolicyTestClient) ResetConnection() {}

func TestDNSOptimizationAnswerPolicyPrecedesCacheAndDoesNotReapplyOnStale(t *testing.T) {
	query, answer := answerPolicyMessage(t, D.TypeA, "www.example. 7200 IN CNAME alias.example.", "alias.example. 7200 IN A 192.0.2.1")
	client := &answerPolicyTestClient{answer: answer}
	r := NewResolverFromClient(client)
	defer r.Close()
	r.answerPolicy = AnswerPolicy{ForceNoCNAME: true, RRTTLMin: 10, RRTTLMax: 20}
	first, err := r.ExchangeContext(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Answer) != 1 || first.Answer[0].Header().Ttl != 20 {
		t.Fatalf("not transformed before delivery: %s", first)
	}
	key := r.cacheKey(context.Background(), query)
	cached, expiry, hit := r.cache.GetWithExpire(key)
	if !hit || len(cached.Answer) != 1 || expiry.After(time.Now().Add(21*time.Second)) {
		t.Fatal("cache did not use transformed answer and lifetime")
	}
	second, err := r.ExchangeContext(context.Background(), query)
	if err != nil || client.calls.Load() != 1 || strings.Contains(second.String(), "CNAME") {
		t.Fatalf("cached answer invalid: %v %v", second, err)
	}
	r.cache.SetWithExpire(key, cached, time.Now().Add(-time.Second))
	stale, err := r.ExchangeContext(context.Background(), query)
	if err != nil || stale.Answer[0].Header().Ttl != 1 {
		t.Fatalf("TTL minimum extended stale reply: %v %v", stale, err)
	}
}

func TestDNSOptimizationAnswerPolicyDoesNotTuneBootstrap(t *testing.T) {
	config := nativeRoutingTestConfig()
	config.AnswerPolicy = AnswerPolicy{ForceNoCNAME: true, RRTTLMax: 3600}
	rs := NewResolver(config)
	defer rs.Close()
	if !rs.Resolver.answerPolicy.ForceNoCNAME || !rs.DirectResolver.answerPolicy.ForceNoCNAME {
		t.Fatal("native answer policy not wired to business resolvers")
	}
	if rs.BootstrapResolver.answerPolicy != (AnswerPolicy{}) || (rs.ProxyResolver != nil && rs.ProxyResolver.answerPolicy != (AnswerPolicy{})) {
		t.Fatal("business answer rewrite leaked into infrastructure DNS")
	}
}

func TestDNSOptimizationAnswerPolicyCacheSeparatesDOAndCD(t *testing.T) {
	for _, mode := range []string{"DO", "CD"} {
		t.Run(mode, func(t *testing.T) {
			query, answer := answerPolicyMessage(t, D.TypeA,
				"www.example. 60 IN CNAME alias.example.", "alias.example. 60 IN A 192.0.2.1")
			client := &answerPolicyTestClient{answer: answer}
			r := NewResolverFromClient(client)
			defer r.Close()
			r.answerPolicy = AnswerPolicy{ForceNoCNAME: true, RRTTLMin: 120}
			ordinary, err := r.ExchangeContext(context.Background(), query)
			if err != nil || len(ordinary.Answer) != 1 {
				t.Fatalf("ordinary query: %v %v", ordinary, err)
			}
			protected := query.Copy()
			if mode == "DO" {
				protected.SetEdns0(1232, true)
			} else {
				protected.CheckingDisabled = true
			}
			result, err := r.ExchangeContext(context.Background(), protected)
			if err != nil || client.calls.Load() != 2 || len(result.Answer) != 2 || result.Answer[0].Header().Ttl != 60 {
				t.Fatalf("protected query reused transformed cache: calls=%d answer=%v err=%v", client.calls.Load(), result, err)
			}
			protected.Id++
			_, err = r.ExchangeContext(context.Background(), protected)
			if err != nil || client.calls.Load() != 2 {
				t.Fatal("transaction ID broke same-policy cache reuse")
			}
		})
	}
}

type answerPolicyFlightClient struct {
	answer  *D.Msg
	started chan bool
	release chan struct{}
}

func (c *answerPolicyFlightClient) ExchangeContext(ctx context.Context, query *D.Msg) (*D.Msg, error) {
	protected := query.CheckingDisabled
	if opt := query.IsEdns0(); opt != nil {
		protected = protected || opt.Do()
	}
	c.started <- protected
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	answer := c.answer.Copy()
	answer.Id = query.Id
	return answer, nil
}
func (*answerPolicyFlightClient) Address() string  { return "answer-policy-flight" }
func (*answerPolicyFlightClient) ResetConnection() {}

func TestDNSOptimizationAnswerPolicyFlightSeparatesProtectedQueries(t *testing.T) {
	for _, mode := range []string{"DO", "CD"} {
		t.Run(mode, func(t *testing.T) {
			query, answer := answerPolicyMessage(t, D.TypeA,
				"www.example. 60 IN CNAME alias.example.", "alias.example. 60 IN A 192.0.2.1")
			client := &answerPolicyFlightClient{answer: answer, started: make(chan bool, 2), release: make(chan struct{})}
			r := NewResolverFromClient(client)
			defer r.Close()
			r.answerPolicy = AnswerPolicy{ForceNoCNAME: true}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			type result struct {
				answer *D.Msg
				err    error
			}
			ordinaryResult, protectedResult := make(chan result, 1), make(chan result, 1)
			go func() { msg, err := r.ExchangeContext(ctx, query); ordinaryResult <- result{msg, err} }()
			select {
			case secure := <-client.started:
				if secure {
					t.Fatal("first query is protected")
				}
			case <-ctx.Done():
				t.Fatal("ordinary query did not start")
			}
			protected := query.Copy()
			if mode == "DO" {
				protected.SetEdns0(1232, true)
			} else {
				protected.CheckingDisabled = true
			}
			go func() { msg, err := r.ExchangeContext(ctx, protected); protectedResult <- result{msg, err} }()
			select {
			case secure := <-client.started:
				if !secure {
					t.Fatal("second flight lost protection flags")
				}
			case <-ctx.Done():
				close(client.release)
				t.Fatal("protected query joined the ordinary singleflight")
			}
			close(client.release)
			first, second := <-ordinaryResult, <-protectedResult
			if first.err != nil || second.err != nil || len(first.answer.Answer) != 1 || len(second.answer.Answer) != 2 {
				t.Fatalf("mixed response policies: %v %v", first, second)
			}
		})
	}
}

func TestDNSOptimizationAnswerPolicyFixedTTLAndReplyCap(t *testing.T) {
	query, answer := answerPolicyMessage(t, D.TypeA, "www.example. 20 IN A 192.0.2.1")
	client := &answerPolicyTestClient{answer: answer}
	r := NewResolverFromClient(client)
	defer r.Close()
	r.answerPolicy = AnswerPolicy{RRTTL: 300, RRTTLMin: 600, RRTTLMax: 60, RRTTLReplyMax: 5}
	for i := 0; i < 2; i++ {
		got, err := r.ExchangeContext(context.Background(), query)
		if err != nil || len(got.Answer) != 1 || got.Answer[0].Header().Ttl != 5 {
			t.Fatalf("reply cap did not apply to live/cache answer: %v %v", got, err)
		}
	}
	key := r.cacheKey(context.Background(), query)
	cached, expiry, hit := r.cache.GetWithExpire(key)
	remaining := time.Until(expiry)
	if !hit || cached.Answer[0].Header().Ttl != 300 || remaining < 298*time.Second || remaining > 301*time.Second || client.calls.Load() != 1 {
		t.Fatalf("reply limit shortened the fixed cache TTL: hit=%t remaining=%s cached=%v calls=%d", hit, remaining, cached, client.calls.Load())
	}
	r.cache.SetWithExpire(key, cached, time.Now().Add(-time.Second))
	stale, err := r.ExchangeContext(context.Background(), query)
	if err != nil || stale.Answer[0].Header().Ttl != 1 {
		t.Fatalf("fixed/reply TTL refilled stale answer: %v %v", stale, err)
	}
}

func TestDNSOptimizationAnswerPolicyFixedTTLPreservesZeroAndAges(t *testing.T) {
	for _, ttl := range []uint32{0, 60} {
		query, answer := answerPolicyMessage(t, D.TypeA, "www.example. 60 IN A 192.0.2.1")
		answer.Answer[0].Header().Ttl = ttl
		ctx := withDNSAnswerLifetimes(context.Background())
		received := time.Now().Add(-2500 * time.Millisecond)
		recordDNSAnswerReceived(ctx, answer, received)
		policy := AnswerPolicy{RRTTL: 10, RRTTLMin: 100, RRTTLMax: 1, RRTTLReplyMax: 5}
		adjusted := policy.apply(query, answer)
		aged := ageDNSAnswerAfterPolicy(ctx, answer, adjusted)
		want := uint32(8)
		if ttl == 0 {
			want = 0
		}
		if aged.Answer[0].Header().Ttl != want || answer.Answer[0].Header().Ttl != ttl {
			t.Fatalf("fixed TTL lost original receipt or rewrote zero: %v", aged)
		}
		if want != 0 && !dnsAnswerExpires(ctx, aged).Equal(received.Add(10*time.Second)) {
			t.Fatal("fixed TTL lost precise receipt-based expiry")
		}
	}
	query := new(D.Msg).SetQuestion("www.example.", D.TypeAAAA)
	transient := dualStackNoData(query, new(D.Msg).SetReply(query))
	if got := (AnswerPolicy{RRTTL: 300, RRTTLReplyMax: 5}).apply(query, transient); got.Ns[0].Header().Ttl != 0 || got.Ns[0].(*D.SOA).Minttl != 0 {
		t.Fatal("fixed TTL made a transient dual-stack preference cacheable")
	}
}

func TestDNSOptimizationAnswerPolicyReplyMaxPreservesProtectedAndEDNS(t *testing.T) {
	query, answer := answerPolicyMessage(t, D.TypeA, "www.example. 60 IN A 192.0.2.1", "www.example. 0 IN A 192.0.2.2")
	answer.SetEdns0(1232, true)
	answer.IsEdns0().SetVersion(1)
	opt := answer.IsEdns0().String()
	p := AnswerPolicy{RRTTLReplyMax: 5}
	got := p.applyReply(query, answer)
	if got.Answer[0].Header().Ttl != 5 || got.Answer[1].Header().Ttl != 0 || got.IsEdns0().String() != opt || answer.Answer[0].Header().Ttl != 60 {
		t.Fatal("reply cap changed zero, EDNS, or shared answer")
	}
	for _, mode := range []string{"DO", "CD", "AD", "RRSIG", "TSIG"} {
		t.Run(mode, func(t *testing.T) {
			q, a := query.Copy(), answer.Copy()
			switch mode {
			case "DO":
				q.SetEdns0(1232, true)
			case "CD":
				q.CheckingDisabled = true
			case "AD":
				a.AuthenticatedData = true
			case "RRSIG":
				a.Answer = append(a.Answer, &D.RRSIG{Hdr: D.RR_Header{Name: "www.example.", Rrtype: D.TypeRRSIG, Class: D.ClassINET, Ttl: 60}})
			case "TSIG":
				a.SetTsig("key.example.", D.HmacSHA256, 300, time.Now().Unix())
			}
			if result := p.applyReply(q, a); result.String() != a.String() {
				t.Fatal("reply cap rewrote a protected transaction")
			}
		})
	}
}

func answerPolicyForcedQtypes(types ...uint16) *ForcedQtypes {
	bitmap := new(ForcedQtypes)
	for _, qtype := range types {
		bitmap[qtype/64] |= uint64(1) << (qtype % 64)
	}
	return bitmap
}

func assertAnswerPolicySOA(t *testing.T, query, answer *D.Msg, ttl uint32) {
	t.Helper()
	if answer == nil || answer.Id != query.Id || answer.Rcode != D.RcodeSuccess || answer.AuthenticatedData || len(answer.Answer) != 0 || len(answer.Ns) != 1 {
		t.Fatalf("forced type did not return local NODATA: %v", answer)
	}
	soa, ok := answer.Ns[0].(*D.SOA)
	if !ok || soa.Hdr.Name != query.Question[0].Name || soa.Hdr.Ttl != ttl || soa.Minttl != ttl {
		t.Fatalf("forced SOA owner/lifetime incorrect: %v", answer)
	}
}

func TestDNSOptimizationForceSOAPrecedesCacheAndExemptsBootstrap(t *testing.T) {
	query, answer := answerPolicyMessage(t, D.TypeAAAA, "www.example. 60 IN AAAA 2001:db8::1")
	client := &answerPolicyTestClient{answer: answer}
	r := NewResolverFromClient(client)
	defer r.Close()
	if _, err := r.ExchangeContext(context.Background(), query); err != nil {
		t.Fatal(err)
	}
	r.answerPolicy.ForceQtypeSOA = answerPolicyForcedQtypes(D.TypeAAAA)
	for i := 0; i < 2; i++ {
		got, err := r.ExchangeContext(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		assertAnswerPolicySOA(t, query, got, 30)
	}
	if client.calls.Load() != 1 {
		t.Fatal("forced SOA queried upstream")
	}
	got, err := r.ExchangeContext(icontext.WithDNSBootstrap(context.Background()), query)
	if err != nil || len(msgToIP(got)) != 1 {
		t.Fatalf("business type filter blocked infrastructure: %v %v", got, err)
	}
	key := r.cacheKey(context.Background(), query)
	if cached, _, hit := r.cache.GetWithExpire(key); !hit || len(msgToIP(cached)) != 1 {
		t.Fatal("forced SOA overwrote a real cache entry")
	}
}

func TestDNSOptimizationForceSOAPolicyRejectDropPrecedence(t *testing.T) {
	routingTestEnable(t)
	for _, kind := range []C.AdapterType{C.Reject, C.RejectDrop} {
		t.Run(kind.String(), func(t *testing.T) {
			leaf := newRoutingTestOutbound("reject")
			leaf.kind.Store(int32(kind))
			cfg := nativeRoutingTestConfig()
			cfg.AnswerPolicy.ForceQtypeSOA = answerPolicyForcedQtypes(D.TypeHTTPS)
			rs := NewResolver(cfg)
			defer rs.Close()
			query := new(D.Msg).SetQuestion("www.example.", D.TypeHTTPS)
			answer, err := rs.Resolver.ExchangeContext(routingTestContext(leaf, routingTestOrigin()), query)
			if kind == C.Reject {
				if err != nil || answer == nil || answer.Rcode != D.RcodeRefused {
					t.Fatalf("forced SOA bypassed REJECT: %v %v", answer, err)
				}
			} else if !errors.Is(err, resolver.ErrDNSDrop) || answer != nil {
				t.Fatalf("forced SOA bypassed REJECT-DROP: %v %v", answer, err)
			}
			if leaf.queryCount.Load() != 0 {
				t.Fatal("blocked query reached upstream")
			}
		})
	}
}

func TestDNSOptimizationForceSOAHostsFakeIPAndObservability(t *testing.T) {
	oldHosts := resolver.DefaultHosts
	t.Cleanup(func() { resolver.DefaultHosts = oldHosts })
	hosts := trie.New[resolver.HostValue]()
	if err := hosts.Insert("hosts.example", resolver.HostValue{IPs: []netip.Addr{netip.MustParseAddr("2001:db8::7")}}); err != nil {
		t.Fatal(err)
	}
	resolver.DefaultHosts = resolver.NewHosts(hosts)
	_, upstream := answerPolicyMessage(t, D.TypeAAAA, "www.example. 60 IN AAAA 2001:db8::1")
	client := &answerPolicyTestClient{answer: upstream}
	r := NewResolverFromClient(client)
	defer r.Close()
	r.answerPolicy = AnswerPolicy{ForceQtypeSOA: answerPolicyForcedQtypes(D.TypeA, D.TypeAAAA, D.TypeHTTPS), RRTTLReplyMax: 5}
	enhancer := fakeIPRoutingTestEnhancer(t)
	enhancer.UseHosts = true
	service := NewService(r, NewEnhancer(enhancer))
	manager := dnsstats.New()
	ctx := observabilityTestContext(manager)
	hostQuery := new(D.Msg).SetQuestion("hosts.example.", D.TypeAAAA)
	got, err := service.ServeMsg(ctx, hostQuery)
	if err != nil || len(msgToIP(got)) != 1 || msgToIP(got)[0].String() != "2001:db8::7" || got.Answer[0].Header().Ttl != 5 {
		t.Fatalf("query-type policy displaced real hosts or lost reply cap: %v %v", got, err)
	}
	for _, qtype := range []uint16{D.TypeA, D.TypeAAAA, D.TypeHTTPS} {
		query := new(D.Msg).SetQuestion("fake.example.", qtype)
		got, err := service.ServeMsg(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		assertAnswerPolicySOA(t, query, got, 5)
	}
	// The dedicated AAAA switch has SmartDNS's stronger pre-hosts priority.
	r.answerPolicy.ForceAAAASOA = true
	service = NewService(r, NewEnhancer(enhancer))
	got, err = service.ServeMsg(ctx, hostQuery)
	if err != nil {
		t.Fatal(err)
	}
	assertAnswerPolicySOA(t, hostQuery, got, 5)
	if stats := manager.Stats(); stats.Totals.Queries != 5 || stats.Totals.Hosts != 1 || stats.Totals.Local != 4 || stats.Totals.Upstream != 0 || stats.Totals.FakeIP != 0 || len(manager.Upstreams().Items) != 0 || client.calls.Load() != 0 {
		t.Fatalf("local type policies were misattributed or performed network work: %+v", stats.Totals)
	}
}

func TestDNSOptimizationForceSOADOAndSignedQueries(t *testing.T) {
	p := AnswerPolicy{ForceAAAASOA: true, RRTTL: 300, RRTTLReplyMax: 5}
	query := new(D.Msg).SetQuestion("www.example.", D.TypeAAAA)
	query.SetEdns0(1232, true)
	query.CheckingDisabled = true
	query.AuthenticatedData = true
	if !forcedSOAQuestion(context.Background(), query) || !p.forcesQtype(D.TypeAAAA) {
		t.Fatal("AD/DO/CD request flags silently bypassed an explicit local block")
	}
	answer := p.forcedSOAReply(context.Background(), query)
	assertAnswerPolicySOA(t, query, answer, 5)
	if answer.IsEdns0() == nil || !answer.IsEdns0().Do() || !answer.CheckingDisabled {
		t.Fatal("local policy reply lost request DO/CD state")
	}
	query.SetTsig("key.example.", D.HmacSHA256, 300, time.Now().Unix())
	if forcedSOAQuestion(context.Background(), query) {
		t.Fatal("local SOA replaced a transaction-authenticated request")
	}
}

func TestDNSOptimizationForceSOADisablesBlockedAuxiliaryFamily(t *testing.T) {
	routingTestEnable(t)
	leaf := newRoutingTestOutbound("direct")
	leaf.answer = fakeIPRoutingTestReply
	cfg := nativeRoutingTestConfig()
	cfg.IPv6 = true
	cfg.SpeedCheck = SpeedCheckConfig{Mode: []string{"tcp:443"}, Timeout: time.Second, ResponseMode: ResponseModeFastestIP}
	cfg.DualStack = DualStackConfig{Enabled: true, AllowForceAAAA: true}
	cfg.AnswerPolicy.ForceAAAASOA = true
	rs := NewResolver(cfg)
	defer rs.Close()
	query := new(D.Msg).SetQuestion("www.example.", D.TypeA)
	got, err := rs.Resolver.ExchangeContext(routingTestContext(leaf, routingTestOrigin()), query)
	if err != nil || len(msgToIP(got)) != 1 || leaf.queryCount.Load() != 1 || leaf.tcp.Load() != 1 {
		t.Fatalf("blocked AAAA was queried/probed or suppressed A: %v %v queries=%d probes=%d", got, err, leaf.queryCount.Load(), leaf.tcp.Load())
	}
}

func TestDNSOptimizationAnswerPolicyHostsAliasFinalReply(t *testing.T) {
	oldHosts := resolver.DefaultHosts
	t.Cleanup(func() { resolver.DefaultHosts = oldHosts })
	hosts := trie.New[resolver.HostValue]()
	if err := hosts.Insert("alias.example", resolver.HostValue{IsDomain: true, Domain: "target.example"}); err != nil {
		t.Fatal(err)
	}
	resolver.DefaultHosts = resolver.NewHosts(hosts)
	for _, flatten := range []bool{false, true} {
		t.Run(map[bool]string{false: "cap", true: "cap-and-flatten"}[flatten], func(t *testing.T) {
			query := new(D.Msg).SetQuestion("alias.example.", D.TypeA)
			client := newCandidateCacheTestClient(func(_ context.Context, q *D.Msg, _ int32) *D.Msg {
				return cacheTestReply(q, 60)
			})
			r := NewResolverFromClient(client)
			defer r.Close()
			r.answerPolicy = AnswerPolicy{ForceNoCNAME: flatten, RRTTL: 300, RRTTLReplyMax: 5}
			service := NewService(r, NewEnhancer(EnhancerConfig{IPv6: true, EnhancedMode: C.DNSNormal, UseHosts: true}))
			for i := 0; i < 2; i++ {
				answer, err := service.ServeMsg(context.Background(), query)
				if err != nil || answer == nil || answer.Question[0].Name != query.Question[0].Name {
					t.Fatalf("alias reply failed: %v %v", answer, err)
				}
				wantCount := 2
				if flatten {
					wantCount = 1
				}
				if len(answer.Answer) != wantCount {
					t.Fatalf("final CNAME policy was lost: %v", answer)
				}
				for _, rr := range answer.Answer {
					if rr.Header().Ttl != 5 || (flatten && (rr.Header().Rrtype != D.TypeA || rr.Header().Name != query.Question[0].Name)) {
						t.Fatalf("post-resolver hosts record escaped final policy: %v", answer)
					}
				}
			}
			targetQuery := new(D.Msg).SetQuestion("target.example.", D.TypeA)
			key := r.cacheKey(context.Background(), targetQuery)
			cached, expiry, hit := r.cache.GetWithExpire(key)
			if !hit || len(cached.Answer) != 1 || cached.Answer[0].Header().Name != "target.example." || cached.Answer[0].Header().Ttl != 300 || time.Until(expiry) < 298*time.Second {
				t.Fatalf("final client alias policy modified target cache: %v", cached)
			}
		})
	}
	query, answer := answerPolicyMessage(t, D.TypeA, "www.example. 10 IN CNAME target.example.", "target.example. 60 IN A 192.0.2.1")
	query.SetEdns0(1232, true)
	before := answer.String()
	if got := (AnswerPolicy{ForceNoCNAME: true, RRTTL: 300, RRTTLReplyMax: 5}).applyFinalReply(query, answer); got.String() != before {
		t.Fatal("final local-alias policy rewrote a protected query")
	}
}

func TestDNSOptimizationForceSOAADQueryCannotBypassListenerPolicy(t *testing.T) {
	for _, p := range []AnswerPolicy{{ForceAAAASOA: true}, {ForceQtypeSOA: answerPolicyForcedQtypes(D.TypeAAAA)}} {
		_, upstream := answerPolicyMessage(t, D.TypeAAAA, "www.example. 60 IN AAAA 2001:db8::1")
		client := &answerPolicyTestClient{answer: upstream}
		r := NewResolverFromClient(client)
		r.answerPolicy = p
		service := NewService(r, NewEnhancer(EnhancerConfig{IPv6: true, EnhancedMode: C.DNSNormal}))
		query := new(D.Msg).SetQuestion("www.example.", D.TypeAAAA)
		query.AuthenticatedData = true
		answer, err := service.ServeMsg(context.Background(), query)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		assertAnswerPolicySOA(t, query, answer, 30)
		if client.calls.Load() != 0 {
			t.Fatal("AD flag bypassed forced SOA and queried upstream")
		}
	}
}

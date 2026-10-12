package dns

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/resolver"
	"github.com/metacubex/mihomo/component/trie"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	D "github.com/miekg/dns"
)

func TestDNSAnswerPolicyADQueryKeepsTTLAndCNAMEPolicy(t *testing.T) {
	query, original := answerPolicyMessage(t, D.TypeA,
		"www.example. 20 IN CNAME target.example.",
		"target.example. 60 IN A 192.0.2.1")
	query.AuthenticatedData = true
	client := &answerPolicyTestClient{answer: original}
	r := NewResolverFromClient(client)
	defer r.Close()
	r.answerPolicy = AnswerPolicy{ForceNoCNAME: true, RRTTL: 300, RRTTLReplyMax: 5}
	for i := 0; i < 2; i++ {
		reply, err := r.ExchangeContext(context.Background(), query)
		if err != nil || len(reply.Answer) != 1 || reply.Answer[0].Header().Rrtype != D.TypeA ||
			reply.Answer[0].Header().Name != query.Question[0].Name || reply.Answer[0].Header().Ttl != 5 {
			t.Fatalf("AD capability bypassed the configured answer policy: %v %v", reply, err)
		}
	}
	cached, _, hit := r.cache.GetWithExpire(r.cacheKey(context.Background(), query))
	if !hit || len(cached.Answer) != 1 || cached.Answer[0].Header().Ttl != 300 || client.calls.Load() != 1 {
		t.Fatalf("AD reply limit leaked into the cache: %v hit=%t calls=%d", cached, hit, client.calls.Load())
	}
	if len(original.Answer) != 2 || original.Answer[0].Header().Ttl != 20 {
		t.Fatal("answer policy changed the shared upstream message")
	}
}

func TestDNSAnswerPolicyForceAAAAFiltersHTTPSHints(t *testing.T) {
	query, original := answerPolicyMessage(t, D.TypeHTTPS,
		"www.example. 120 IN HTTPS 1 . mandatory=alpn,ipv6hint,key65400 alpn=\"h2,h3\" no-default-alpn port=8443 ipv4hint=192.0.2.1 ech=AAE= ipv6hint=2001:db8::1,2001:db8::2 key65400=\"opaque\"")
	query.AuthenticatedData = true // Capability requests still receive local policy.
	before := original.String()
	policy := AnswerPolicy{ForceAAAASOA: true}
	reply := policy.apply(query, original)
	record := reply.Answer[0].(*D.HTTPS)
	if record.Priority != 1 || record.Target != "." || record.Hdr.Ttl != 120 {
		t.Fatalf("HTTPS metadata changed: %v", record)
	}
	originalParams := make(map[D.SVCBKey]string)
	for _, param := range original.Answer[0].(*D.HTTPS).Value {
		originalParams[param.Key()] = param.String()
	}
	for _, param := range record.Value {
		switch value := param.(type) {
		case *D.SVCBIPv6Hint:
			t.Fatal("forced AAAA policy left HTTPS IPv6 hints")
		case *D.SVCBMandatory:
			if !reflect.DeepEqual(value.Code, []D.SVCBKey{D.SVCB_ALPN, 65400}) {
				t.Fatalf("mandatory no longer names exactly the retained parameters: %v", value)
			}
		default:
			if param.String() != originalParams[param.Key()] {
				t.Fatalf("unrelated HTTPS parameter changed: %s", param.Key())
			}
		}
	}
	if len(record.Value) != len(originalParams)-1 || original.String() != before {
		t.Fatal("HTTPS filtering discarded another parameter or mutated the original")
	}
	wire, err := reply.Pack()
	if err != nil {
		t.Fatal(err)
	}
	var decoded D.Msg
	if err := decoded.Unpack(wire); err != nil {
		t.Fatalf("filtered HTTPS message cannot round-trip on the wire: %v", err)
	}
	query, answer := answerPolicyMessage(t, D.TypeHTTPS,
		"www.example. 120 IN HTTPS 1 . mandatory=ipv6hint ipv6hint=2001:db8::1")
	if got := policy.apply(query, answer); len(got.Answer[0].(*D.HTTPS).Value) != 0 {
		t.Fatalf("an empty mandatory parameter was retained: %v", got)
	}
	for _, p := range []AnswerPolicy{{}, {ForceQtypeSOA: answerPolicyForcedQtypes(D.TypeAAAA)}} {
		if got := p.apply(query, answer); got.String() != answer.String() {
			t.Fatal("generic qtype 28 blocking implied HTTPS address filtering")
		}
	}
	for _, protected := range []string{"DO", "CD", "response-AD", "RRSIG"} {
		t.Run(protected, func(t *testing.T) {
			q, a := query.Copy(), answer.Copy()
			switch protected {
			case "DO":
				q.SetEdns0(1232, true)
			case "CD":
				q.CheckingDisabled = true
			case "response-AD":
				a.AuthenticatedData = true
			case "RRSIG":
				a.Answer = append(a.Answer, &D.RRSIG{Hdr: D.RR_Header{Name: "www.example.", Rrtype: D.TypeRRSIG, Class: D.ClassINET, Ttl: 120}})
			}
			if got := policy.apply(q, a); got.String() != a.String() {
				t.Fatal("HTTPS filtering rewrote a protected reply")
			}
		})
	}
}

func TestDNSAnswerPolicyForceAAAAHTTPSCacheAndBootstrap(t *testing.T) {
	client := newCandidateCacheTestClient(func(_ context.Context, q *D.Msg, _ int32) *D.Msg {
		answer := new(D.Msg).SetReply(q)
		answer.Answer = []D.RR{speedCheckTestRR(t, "www.example. 120 IN HTTPS 1 . alpn=\"h2\" ipv6hint=2001:db8::1")}
		return answer
	})
	r := NewResolverFromClient(client)
	defer r.Close()
	r.answerPolicy = AnswerPolicy{ForceAAAASOA: true}
	query := new(D.Msg).SetQuestion("www.example.", D.TypeHTTPS)
	for i := 0; i < 2; i++ {
		reply, err := r.ExchangeContext(context.Background(), query)
		if err != nil || len(reply.Answer[0].(*D.HTTPS).Value) != 1 {
			t.Fatalf("HTTPS cache returned a filtered hint: %v %v", reply, err)
		}
	}
	if client.calls.Load() != 1 {
		t.Fatal("HTTPS filtered answers were not cached")
	}
	query.SetEdns0(1232, true)
	reply, err := r.ExchangeContext(context.Background(), query)
	if err != nil || len(reply.Answer[0].(*D.HTTPS).Value) != 2 || client.calls.Load() != 2 {
		t.Fatalf("DO request reused the unsigned filtered cache: %v %v", reply, err)
	}
	config := selectionTestConfig()
	config.AnswerPolicy = AnswerPolicy{ForceAAAASOA: true}
	rs := NewResolver(config)
	defer rs.Close()
	if rs.BootstrapResolver.answerPolicy.ForceAAAASOA || (rs.ProxyResolver != nil && rs.ProxyResolver.answerPolicy.ForceAAAASOA) {
		t.Fatal("HTTPS business policy escaped into infrastructure resolution")
	}
}

func negativePolicyReply(query *D.Msg, rcode int, headerTTL, minimum uint32) *D.Msg {
	answer := new(D.Msg).SetRcode(query, rcode)
	answer.Ns = []D.RR{&D.SOA{
		Hdr: D.RR_Header{Name: "example.", Rrtype: D.TypeSOA, Class: D.ClassINET, Ttl: headerTTL},
		Ns:  "ns.example.", Mbox: "hostmaster.example.", Minttl: minimum,
	}}
	return answer
}

func TestDNSNegativeCacheUsesSOAMinimumAndChainTTL(t *testing.T) {
	query := new(D.Msg).SetQuestion("www.example.", D.TypeA)
	for _, test := range []struct {
		name        string
		rcode       int
		header, min uint32
		chain, want uint32
		positive    bool
	}{
		{"NXDOMAIN minimum", D.RcodeNameError, 600, 60, 0, 60, false},
		{"NODATA minimum", D.RcodeSuccess, 600, 60, 0, 60, false},
		{"SOA header wins", D.RcodeNameError, 20, 60, 0, 20, false},
		{"NXDOMAIN CNAME bounds", D.RcodeNameError, 600, 60, 15, 15, false},
		{"NODATA CNAME bounds", D.RcodeSuccess, 600, 60, 15, 15, false},
		{"zero minimum", D.RcodeNameError, 600, 0, 0, 0, false},
		{"zero SOA header", D.RcodeSuccess, 0, 60, 0, 0, false},
		{"positive SOA is not negative", D.RcodeSuccess, 600, 0, 0, 300, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			answer := negativePolicyReply(query, test.rcode, test.header, test.min)
			if test.chain != 0 {
				answer.Answer = []D.RR{&D.CNAME{
					Hdr:    D.RR_Header{Name: query.Question[0].Name, Rrtype: D.TypeCNAME, Class: D.ClassINET, Ttl: test.chain},
					Target: "terminal.example.",
				}}
			}
			if test.positive {
				answer.Answer = cacheTestReply(query, 300).Answer
			}
			if ttl := dnsMessageCacheTTL(answer); ttl != test.want {
				t.Fatalf("negative cache TTL=%d, want %d", ttl, test.want)
			}
		})
	}
}

func TestDNSNegativeCacheAgesMinimumBeforeStore(t *testing.T) {
	query := new(D.Msg).SetQuestion("www.example.", D.TypeAAAA)
	original := negativePolicyReply(query, D.RcodeSuccess, 600, 30)
	signature := &D.RRSIG{
		Hdr:         D.RR_Header{Name: "example.", Rrtype: D.TypeRRSIG, Class: D.ClassINET, Ttl: 600},
		TypeCovered: D.TypeSOA, OrigTtl: 600, Signature: "AA==",
	}
	original.Ns = append(original.Ns, signature)
	ctx := withDNSAnswerLifetimes(context.Background())
	receivedAt := time.Now().Add(-2500 * time.Millisecond)
	recordDNSAnswerReceived(ctx, original, receivedAt)
	aged := ageDNSAnswerAfterPolicy(ctx, original, original)
	if aged.Ns[0].Header().Ttl != 28 || aged.Ns[0].(*D.SOA).Minttl != 30 ||
		aged.Ns[1].(*D.RRSIG).OrigTtl != 600 || aged.Ns[1].(*D.RRSIG).Signature != "AA==" ||
		original.Ns[0].Header().Ttl != 600 {
		t.Fatalf("negative aging reset a lifetime or changed signed RDATA: %v", aged)
	}
	wantExpiry := receivedAt.Add(30 * time.Second)
	if expiry := dnsAnswerExpires(ctx, aged); !expiry.Equal(wantExpiry) {
		t.Fatalf("negative expiry restarted after waiting: %s want %s", expiry, wantExpiry)
	}
	for _, algorithm := range []string{"lru", "arc"} {
		t.Run(algorithm, func(t *testing.T) {
			cache := Config{CacheAlgorithm: algorithm}.newCache()
			putMsgToCacheWithExpiry(cache, "negative", query.Question[0], aged, wantExpiry)
			cached, expires, hit := cache.GetWithExpire("negative")
			if !hit || expires.After(wantExpiry) || expires.Before(wantExpiry.Truncate(time.Second)) {
				t.Fatalf("negative cache exceeded SOA.MINIMUM: hit=%t expires=%s want<=%s", hit, expires, wantExpiry)
			}
			cached = cached.Copy()
			remaining := uint32(time.Until(expires) / time.Second)
			updateMsgTTL(cached, remaining)
			if cached.Ns[0].Header().Ttl > remaining || cached.Ns[0].(*D.SOA).Minttl != 30 {
				t.Fatalf("negative cache hit advertised a renewed lifetime: %v", cached)
			}
			zero := negativePolicyReply(query, D.RcodeSuccess, 600, 0)
			zero = (AnswerPolicy{RRTTL: 300, RRTTLMin: 600}).apply(query, zero)
			putMsgToCache(cache, "negative", query.Question[0], zero)
			if _, _, hit := cache.GetWithExpire("negative"); hit {
				t.Fatal("zero negative lifetime retained an older serve-expired answer")
			}
		})
	}
}

func TestDNSAnswerPolicyHostsNegativeAliasIdentity(t *testing.T) {
	oldHosts := resolver.DefaultHosts
	t.Cleanup(func() { resolver.DefaultHosts = oldHosts })
	hosts := trie.New[resolver.HostValue]()
	if err := hosts.Insert("alias.example", resolver.HostValue{IsDomain: true, Domain: "target.example"}); err != nil {
		t.Fatal(err)
	}
	resolver.DefaultHosts = resolver.NewHosts(hosts)
	for _, test := range []struct {
		name    string
		force   bool
		rcode   int
		remote  bool
		answers int
	}{
		{"default NODATA", false, D.RcodeSuccess, false, 1},
		{"hidden NODATA alias", true, D.RcodeSuccess, false, 0},
		{"NXDOMAIN terminal identity", true, D.RcodeNameError, true, 2},
		{"NODATA remote chain", true, D.RcodeSuccess, true, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newCandidateCacheTestClient(func(_ context.Context, q *D.Msg, _ int32) *D.Msg {
				answer := negativePolicyReply(q, test.rcode, 60, 30)
				if test.remote {
					answer.Answer = []D.RR{speedCheckTestRR(t, "target.example. 20 IN CNAME terminal.example.")}
				}
				return answer
			})
			r := NewResolverFromClient(client)
			defer r.Close()
			r.answerPolicy = AnswerPolicy{ForceNoCNAME: test.force, RRTTLReplyMax: 5}
			service := NewService(r, NewEnhancer(EnhancerConfig{IPv6: true, EnhancedMode: C.DNSNormal, UseHosts: true}))
			query := new(D.Msg).SetQuestion("alias.example.", D.TypeA)
			for i := 0; i < 2; i++ {
				reply, err := service.ServeMsg(context.Background(), query)
				if err != nil || reply.Rcode != test.rcode || reply.Question[0] != query.Question[0] || len(reply.Answer) != test.answers {
					t.Fatalf("local alias corrupted a negative answer: %v %v", reply, err)
				}
				if test.remote && (reply.Answer[0].(*D.CNAME).Hdr.Name != "alias.example." ||
					reply.Answer[0].(*D.CNAME).Target != "target.example." ||
					reply.Answer[1].(*D.CNAME).Target != "terminal.example.") {
					t.Fatalf("negative answer lost the terminal name: %v", reply)
				}
				if reply.Ns[0].Header().Ttl > 5 {
					t.Fatalf("negative alias escaped the final reply cap: %v", reply)
				}
			}
			target := new(D.Msg).SetQuestion("target.example.", D.TypeA)
			cached, _, hit := r.cache.GetWithExpire(r.cacheKey(context.Background(), target))
			if !hit || cached.Question[0] != target.Question[0] || client.calls.Load() != 1 ||
				(len(cached.Answer) != 0 && cached.Answer[0].Header().Name == "alias.example.") {
				t.Fatalf("local alias modified the target cache: %v hit=%t calls=%d", cached, hit, client.calls.Load())
			}
		})
	}
}

func TestDNSAnswerPolicyHostsExplicitReplyLimit(t *testing.T) {
	oldHosts := resolver.DefaultHosts
	t.Cleanup(func() { resolver.DefaultHosts = oldHosts })
	hosts := trie.New[resolver.HostValue]()
	var addresses []netip.Addr
	for i := 1; i <= 12; i++ {
		addresses = append(addresses, netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", i)), netip.MustParseAddr(fmt.Sprintf("2001:db8::%x", i)))
	}
	if err := hosts.Insert("static.example", resolver.HostValue{IPs: addresses}); err != nil {
		t.Fatal(err)
	}
	resolver.DefaultHosts = resolver.NewHosts(hosts)
	for _, qtype := range []uint16{D.TypeA, D.TypeAAAA} {
		for _, test := range []struct {
			name    string
			limit   int
			want    int
			protect string
		}{
			{"default preserves all", 0, 12, ""},
			{"explicit limit", 3, 3, ""},
			{"larger than answer", 32, 12, ""},
			{"AD capability", 3, 3, "AD"},
			{"DO preserved", 3, 12, "DO"},
			{"CD preserved", 3, 12, "CD"},
			{"bootstrap context", 3, 12, "bootstrap-context"},
			{"bootstrap resolver", 3, 12, "bootstrap-resolver"},
		} {
			t.Run(D.TypeToString[qtype]+"/"+test.name, func(t *testing.T) {
				client := newCandidateCacheTestClient(func(_ context.Context, q *D.Msg, _ int32) *D.Msg {
					return cacheTestReply(q, 60)
				})
				r := NewResolverFromClient(client)
				defer r.Close()
				r.answerPolicy = AnswerPolicy{ReplyIPLimit: test.limit}
				r.bootstrap = test.protect == "bootstrap-resolver"
				service := NewService(r, NewEnhancer(EnhancerConfig{IPv6: true, EnhancedMode: C.DNSNormal, UseHosts: true}))
				query := new(D.Msg).SetQuestion("static.example.", qtype)
				ctx := context.Background()
				switch test.protect {
				case "AD":
					query.AuthenticatedData = true
				case "DO":
					query.SetEdns0(1232, true)
				case "CD":
					query.CheckingDisabled = true
				case "bootstrap-context":
					ctx = icontext.WithDNSBootstrap(ctx)
				}
				for i := 0; i < 2; i++ {
					query.Id++
					answer, err := service.ServeMsg(ctx, query)
					if err != nil || answer == nil || answer.Id != query.Id || len(msgToIP(answer)) != test.want || answer.AuthenticatedData {
						t.Fatalf("static hosts limit/protection changed: want=%d answer=%v err=%v", test.want, answer, err)
					}
					for _, rr := range answer.Answer {
						if rr.Header().Name != query.Question[0].Name || rr.Header().Rrtype != qtype || rr.Header().Ttl != 10 {
							t.Fatalf("limiting addresses changed hosts record identity or TTL: %v", rr)
						}
					}
				}
				stored, found := resolver.DefaultHosts.Search("static.example", false)
				if !found || !reflect.DeepEqual(stored.IPs, addresses) || client.calls.Load() != 0 || r.speedChecker != nil {
					t.Fatal("hosts limit mutated the source or started upstream/probe work")
				}
			})
		}
	}
}

func TestDNSAnswerPolicyReplyLimitPreservesFakeIP(t *testing.T) {
	client := newCandidateCacheTestClient(func(_ context.Context, q *D.Msg, _ int32) *D.Msg {
		return cacheTestReply(q, 60)
	})
	r := NewResolverFromClient(client)
	defer r.Close()
	r.answerPolicy = AnswerPolicy{ReplyIPLimit: 3}
	service := NewService(r, NewEnhancer(fakeIPRoutingTestEnhancer(t)))
	for _, qtype := range []uint16{D.TypeA, D.TypeAAAA} {
		query := new(D.Msg).SetQuestion("fake-limit.example.", qtype)
		query.AuthenticatedData = true
		answer, err := service.ServeMsg(context.Background(), query)
		if err != nil || answer == nil || len(msgToIP(answer)) != 1 || answer.Answer[0].Header().Ttl != 17 || answer.AuthenticatedData {
			t.Fatalf("reply limit changed FakeIP synthesis: %v %v", answer, err)
		}
	}
	if client.calls.Load() != 0 {
		t.Fatal("local FakeIP reply invoked an upstream")
	}
}

package dns

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

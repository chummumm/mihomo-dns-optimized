package dns

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/common/lru"
	"github.com/metacubex/mihomo/component/fakeip"
	"github.com/metacubex/mihomo/component/resolver"
	"github.com/metacubex/mihomo/component/trie"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"

	D "github.com/miekg/dns"
)

func redirHostTestFilter(t testing.TB, patterns ...string) *trie.DomainSet {
	t.Helper()
	var builder trie.DomainSetBuilder
	for _, pattern := range patterns {
		if err := builder.Insert(pattern); err != nil {
			t.Fatal(err)
		}
	}
	return builder.Build()
}

func redirHostTestMapper(filter *trie.DomainSet) *ResolverEnhancer {
	return NewEnhancer(EnhancerConfig{
		EnhancedMode: C.DNSMapping, IPv6: true, RedirHostFilter: filter,
	})
}

func redirHostTestReply(query *D.Msg) *D.Msg {
	reply := new(D.Msg).SetReply(query)
	header := D.RR_Header{Name: query.Question[0].Name, Rrtype: query.Question[0].Qtype, Class: D.ClassINET, Ttl: 120}
	if header.Rrtype == D.TypeAAAA {
		reply.Answer = []D.RR{&D.AAAA{Hdr: header, AAAA: net.ParseIP("2001:db8::19")}}
	} else {
		reply.Answer = []D.RR{&D.A{Hdr: header, A: net.ParseIP("192.0.2.19")}}
	}
	return reply
}

type redirHostTestClient struct {
	calls atomic.Int32
}

func (c *redirHostTestClient) ExchangeContext(_ context.Context, query *D.Msg) (*D.Msg, error) {
	c.calls.Add(1)
	return redirHostTestReply(query), nil
}

func (*redirHostTestClient) Address() string  { return "test://redir-host-filter" }
func (*redirHostTestClient) ResetConnection() {}

func TestRedirHostFilterColdAndCachedAnswers(t *testing.T) {
	filter := redirHostTestFilter(t, "blocked.example", "+.suffix.example")
	for _, name := range []string{"Blocked.Example.", "sub.suffix.example.", "allowed.example."} {
		for _, qtype := range []uint16{D.TypeA, D.TypeAAAA} {
			t.Run(fmt.Sprintf("%s/%s", name, D.TypeToString[qtype]), func(t *testing.T) {
				mapper := redirHostTestMapper(filter)
				client := &redirHostTestClient{}
				r := &Resolver{main: []dnsClient{client}, cache: Config{}.newCache()}
				service := NewService(r, mapper)
				query := new(D.Msg).SetQuestion(name, qtype)
				query.SetEdns0(1232, true)
				for attempt := 0; attempt < 2; attempt++ {
					// Force each response, including a DNS answer cache hit, to
					// establish its own mapping instead of reusing the first one.
					mapper.mapping.Clear()
					query.Id++
					reply, err := service.ServeMsg(context.Background(), query)
					if err != nil || reply == nil || reply.Id != query.Id || reply.Rcode != D.RcodeSuccess {
						t.Fatalf("attempt %d lost the DNS response: %v, %v", attempt, reply, err)
					}
					ips := msgToIP(reply)
					if len(ips) != 1 || reply.Answer[0].Header().Ttl == 0 {
						t.Fatalf("attempt %d lost the address or TTL: %v", attempt, reply)
					}
					if opt := reply.IsEdns0(); opt == nil || !opt.Do() || opt.UDPSize() != 1232 {
						t.Fatalf("attempt %d lost the requested EDNS behavior: %v", attempt, reply)
					}
					wantMapping := name == "allowed.example."
					host, mapped := mapper.FindHostByIP(ips[0])
					if mapped != wantMapping || (mapped && host != "allowed.example") {
						t.Fatalf("attempt %d mapped %v to %q, %t", attempt, ips[0], host, mapped)
					}
					if mapper.mapping.Exist(ips[0]) != wantMapping {
						t.Fatal("excluded answer was stored behind the read filter")
					}
				}
				if calls := client.calls.Load(); calls != 1 {
					t.Fatalf("the second answer did not use the DNS cache: %d upstream exchanges", calls)
				}
			})
		}
	}
}

func TestRedirHostFilterPreservesCNAMEAnswersAndEDNS(t *testing.T) {
	for _, queryName := range []string{"blocked.example.", "allowed.example."} {
		t.Run(queryName, func(t *testing.T) {
			mapper := redirHostTestMapper(redirHostTestFilter(t, "blocked.example", "target.example"))
			query := new(D.Msg).SetQuestion(queryName, D.TypeA)
			reply := new(D.Msg).SetReply(query)
			reply.Answer = []D.RR{
				&D.CNAME{Hdr: D.RR_Header{Name: queryName, Rrtype: D.TypeCNAME, Class: D.ClassINET, Ttl: 90}, Target: "target.example."},
				&D.A{Hdr: D.RR_Header{Name: "target.example.", Rrtype: D.TypeA, Class: D.ClassINET, Ttl: 0}, A: net.ParseIP("192.0.2.19")},
				&D.AAAA{Hdr: D.RR_Header{Name: "target.example.", Rrtype: D.TypeAAAA, Class: D.ClassINET, Ttl: 60}, AAAA: net.ParseIP("2001:db8::19")},
			}
			reply.SetEdns0(4096, true)
			reply.IsEdns0().SetVersion(1)
			reply.IsEdns0().SetExtendedRcode(D.RcodeBadVers)
			reply.IsEdns0().Option = []D.EDNS0{&D.EDNS0_COOKIE{Code: D.EDNS0COOKIE, Cookie: "0123456789abcdef"}}
			before, err := reply.Copy().Pack()
			if err != nil {
				t.Fatal(err)
			}
			beforeOPTHeader := reply.IsEdns0().Hdr
			h := withMapping(mapper)(func(_ *icontext.DNSContext, _ *D.Msg) (*D.Msg, error) {
				return reply, nil
			})
			got, err := h(icontext.NewDNSContext(context.Background()), query)
			if err != nil || got == nil {
				t.Fatalf("mapping filtering lost the response: %v, %v", got, err)
			}
			after, err := got.Copy().Pack()
			if err != nil || !bytes.Equal(after, before) || got.IsEdns0().Hdr != beforeOPTHeader {
				t.Fatalf("mapping filtering changed DNS records, TTL or OPT: %v, %v", got, err)
			}
			for _, ip := range []string{"192.0.2.19", "2001:db8::19"} {
				host, ok := mapper.FindHostByIP(netip.MustParseAddr(ip))
				if queryName == "blocked.example." {
					if ok {
						t.Fatalf("blocked query mapped to %q", host)
					}
				} else if !ok || host != "allowed.example" {
					t.Fatalf("a blocked CNAME target suppressed the allowed query label: %q, %t", host, ok)
				}
			}
		})
	}
}

func TestRedirHostFilterHostsAndAliases(t *testing.T) {
	oldHosts := resolver.DefaultHosts
	t.Cleanup(func() { resolver.DefaultHosts = oldHosts })
	hosts := trie.New[resolver.HostValue]()
	ipv4, ipv6 := netip.MustParseAddr("192.0.2.19"), netip.MustParseAddr("2001:db8::19")
	for name, value := range map[string]resolver.HostValue{
		"blocked.example":       {IPs: []netip.Addr{ipv4, ipv6}},
		"allowed.example":       {IPs: []netip.Addr{ipv4, ipv6}},
		"blocked-alias.example": {IsDomain: true, Domain: "allowed-target.example"},
		"allowed-alias.example": {IsDomain: true, Domain: "blocked-target.example"},
		"allowed-open.example":  {IsDomain: true, Domain: "allowed-target.example"},
		"blocked-local.example": {IsDomain: true, Domain: "allowed.example"},
		"allowed-local.example": {IsDomain: true, Domain: "blocked.example"},
	} {
		if err := hosts.Insert(name, value); err != nil {
			t.Fatal(err)
		}
	}
	resolver.DefaultHosts = resolver.NewHosts(hosts)
	filter := redirHostTestFilter(t, "blocked.example", "blocked-alias.example", "blocked-target.example", "blocked-local.example")
	for _, test := range []struct {
		query, target, mapped string
	}{
		{query: "blocked.example"},
		{query: "allowed.example", mapped: "allowed.example"},
		{query: "blocked-alias.example", target: "allowed-target.example"},
		{query: "allowed-alias.example", target: "blocked-target.example"},
		{query: "allowed-open.example", target: "allowed-target.example", mapped: "allowed-target.example"},
		{query: "blocked-local.example"},
		{query: "allowed-local.example", mapped: "allowed-local.example"},
	} {
		for _, qtype := range []uint16{D.TypeA, D.TypeAAAA} {
			t.Run(fmt.Sprintf("%s/%s", test.query, D.TypeToString[qtype]), func(t *testing.T) {
				mapper := redirHostTestMapper(filter)
				nextCalls := 0
				h := compose([]middleware{withHosts(mapper), withMapping(mapper)}, func(ctx *icontext.DNSContext, query *D.Msg) (*D.Msg, error) {
					nextCalls++
					if query.Question[0].Name != test.target+"." {
						t.Fatalf("unexpected alias resolution: %s", query.Question[0].Name)
					}
					ctx.SetType(icontext.DNSTypeRaw)
					return redirHostTestReply(query), nil
				})
				query := new(D.Msg).SetQuestion(test.query+".", qtype)
				ctx := icontext.NewDNSContext(context.Background())
				reply, err := h(ctx, query)
				if err != nil || reply == nil || !reflect.DeepEqual(reply.Question, query.Question) || reply.Id != query.Id {
					t.Fatalf("hosts response changed original question: %v, %v", reply, err)
				}
				ips := msgToIP(reply)
				if len(ips) != 1 {
					t.Fatalf("hosts response lost its address: %v", reply)
				}
				host, ok := mapper.FindHostByIP(ips[0])
				if ok != (test.mapped != "") || host != test.mapped || mapper.mapping.Exist(ips[0]) != ok {
					t.Fatalf("incorrect hosts mapping: got %q, %t; want %q", host, ok, test.mapped)
				}
				if test.target == "" {
					if nextCalls != 0 || ctx.Type() != icontext.DNSTypeHost || reply.Answer[0].Header().Ttl != 10 {
						t.Fatalf("static hosts behavior changed: calls=%d type=%s reply=%v", nextCalls, ctx.Type(), reply)
					}
				} else {
					cname, isCNAME := reply.Answer[0].(*D.CNAME)
					if nextCalls != 1 || ctx.Type() != icontext.DNSTypeRaw || !isCNAME || cname.Target != test.target+"." || cname.Hdr.Ttl != 10 {
						t.Fatalf("alias response or context type changed: calls=%d type=%s reply=%v", nextCalls, ctx.Type(), reply)
					}
				}
			})
		}
	}
}

func TestRedirHostFilterMappingAccessAndSharedIP(t *testing.T) {
	filter := redirHostTestFilter(t, "blocked.example", "+.suffix.example", "*.single.example")
	ip := netip.MustParseAddr("192.0.2.19")
	for _, test := range []struct {
		host string
		want bool
	}{
		{"Blocked.Example.", false},
		{"suffix.example", false},
		{"deep.sub.suffix.example.", false},
		{"a.single.example.", false},
		{"single.example", true},
		{"deep.a.single.example", true},
		{"notblocked.example", true},
	} {
		t.Run(test.host, func(t *testing.T) {
			mapper := redirHostTestMapper(filter)
			mapper.InsertHostByIP(ip, test.host)
			host, ok := mapper.FindHostByIP(ip)
			if ok != test.want || mapper.mapping.Exist(ip) != test.want || (ok && host != test.host) {
				t.Fatalf("InsertHostByIP(%q) returned %q, %t", test.host, host, ok)
			}
			mapper.mapping.Set(ip, test.host) // Simulate a retained legacy entry.
			if _, ok := mapper.FindHostByIP(ip); ok != test.want {
				t.Fatal("reading a legacy entry bypassed the current filter")
			}
		})
	}

	mapper := redirHostTestMapper(filter)
	mapper.InsertHostByIP(ip, "allowed.example")
	mapper.InsertHostByIP(ip, "blocked.example")
	query := new(D.Msg).SetQuestion("blocked.example.", D.TypeA)
	h := withMapping(mapper)(func(_ *icontext.DNSContext, query *D.Msg) (*D.Msg, error) { return redirHostTestReply(query), nil })
	if _, err := h(icontext.NewDNSContext(context.Background()), query); err != nil {
		t.Fatal(err)
	}
	if host, ok := mapper.FindHostByIP(ip); !ok || host != "allowed.example" {
		t.Fatalf("a blocked query suppressed another allowed name sharing its IP: %q, %t", host, ok)
	}
}

func TestRedirHostFilterReloadPreservesAllowedEntries(t *testing.T) {
	old := redirHostTestMapper(nil)
	first, blocked, last := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("2001:db8::3")
	expires := time.Now().Add(7 * time.Minute).Truncate(time.Second)
	old.mapping.SetWithExpire(first, "first.example", expires)
	old.mapping.SetWithExpire(blocked, "Blocked.Example.", expires.Add(time.Minute))
	old.mapping.SetWithExpire(last, "last.example", expires.Add(2*time.Minute))
	current := redirHostTestMapper(redirHostTestFilter(t, "blocked.example"))
	current.mapping = lru.New(lru.WithSize[netip.Addr, string](2))
	current.PatchFrom(old)
	orderCheck := redirHostTestMapper(current.redirHostFilter)
	orderCheck.mapping = lru.New(lru.WithSize[netip.Addr, string](2))
	orderCheck.PatchFrom(old)
	if current.mapping.Exist(blocked) {
		t.Fatal("reload retained an excluded old mapping")
	}
	if !old.mapping.Exist(blocked) {
		t.Fatal("reload mutated the old mapper")
	}
	// An old in-flight DNS response must not mutate the cloned entry or its
	// absolute expiry after the new mapper is installed.
	old.mapping.SetWithExpire(first, "changed.example", expires.Add(time.Hour))
	if host, expiry, ok := current.mapping.GetWithExpire(first); !ok || host != "first.example" || !expiry.Equal(expires) {
		t.Fatalf("reload shared or refreshed an allowed entry: %q, %v, %t", host, expiry, ok)
	}
	if host, expiry, ok := current.mapping.GetWithExpire(last); !ok || host != "last.example" || !expiry.Equal(expires.Add(2*time.Minute)) {
		t.Fatalf("reload changed IPv6 mapping lifetime: %q, %v, %t", host, expiry, ok)
	}
	// Probe an untouched clone so the TTL checks cannot repair a reversed LRU.
	orderCheck.InsertHostByIP(netip.MustParseAddr("192.0.2.4"), "new.example")
	if orderCheck.mapping.Exist(first) || !orderCheck.mapping.Exist(last) {
		t.Fatal("filtered reload lost relative LRU order")
	}

	removed := redirHostTestMapper(nil)
	removed.PatchFrom(current)
	removed.InsertHostByIP(blocked, "blocked.example")
	if host, ok := removed.FindHostByIP(blocked); !ok || host != "blocked.example" {
		t.Fatal("removing the filter did not permit new mapping writes")
	}
}

func TestRedirHostFilterModeIsolation(t *testing.T) {
	filter := redirHostTestFilter(t, "blocked.example")
	query := new(D.Msg).SetQuestion("blocked.example.", D.TypeA)
	ip := netip.MustParseAddr("192.0.2.19")
	for _, mode := range []C.DNSMode{C.DNSNormal, C.DNSMapping, C.DNSFakeIP} {
		t.Run(mode.String(), func(t *testing.T) {
			mapper := NewEnhancer(EnhancerConfig{EnhancedMode: mode, RedirHostFilter: filter})
			mapper.InsertHostByIP(ip, "blocked.example")
			if _, ok := mapper.FindHostByIP(ip); ok != (mode == C.DNSFakeIP) {
				t.Fatalf("filter changed mode %s real-IP mapping behavior", mode)
			}
		})
	}
	pool, err := fakeip.New(fakeip.Options{IPNet: netip.MustParsePrefix("198.18.0.1/16"), Size: 16})
	if err != nil {
		t.Fatal(err)
	}
	mapper := NewEnhancer(EnhancerConfig{
		EnhancedMode: C.DNSFakeIP, FakeIPPool: pool, FakeIPSkipper: &fakeip.Skipper{}, FakeIPTTL: 17, RedirHostFilter: filter,
	})
	service := NewService(nil, mapper)
	reply, err := service.ServeMsg(context.Background(), query)
	if err != nil || len(reply.Answer) != 1 || reply.Answer[0].Header().Ttl != 17 {
		t.Fatalf("filter changed fake-IP response: %v, %v", reply, err)
	}
	fake := msgToIP(reply)[0]
	if host, ok := mapper.FindHostByIP(fake); !ok || host != "blocked.example" || !pool.IPNet().Contains(fake) {
		t.Fatalf("filter changed fake-IP reverse mapping: %q, %t, %v", host, ok, fake)
	}
}

func BenchmarkRedirHostFilter(b *testing.B) {
	for _, size := range []int{0, 100, 10_000, 100_000} {
		b.Run(fmt.Sprintf("patterns=%d", size), func(b *testing.B) {
			var builder trie.DomainSetBuilder
			for i := 0; i < size; i++ {
				if err := builder.Insert(fmt.Sprintf("entry%06d.bench.example", i)); err != nil {
					b.Fatal(err)
				}
			}
			if size > 0 {
				if err := builder.Insert("+.suffix.bench.example"); err != nil {
					b.Fatal(err)
				}
			}
			mapper := redirHostTestMapper(builder.Build())
			for _, test := range []struct {
				name, domain string
				wantAllowed  bool
			}{
				{"exact", "Entry000000.Bench.Example.", size == 0},
				{"suffix", "deep.sub.suffix.bench.example.", size == 0},
				{"miss", "missing.bench.example.", true},
			} {
				b.Run(test.name, func(b *testing.B) {
					b.ReportAllocs()
					var allowed bool
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						allowed = mapper.mappingAllowed(test.domain)
					}
					b.StopTimer()
					if allowed != test.wantAllowed {
						b.Fatalf("incorrect match for %s: allowed=%t", test.domain, allowed)
					}
				})
			}
		})
	}
}

package dns

import (
	"context"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
	D "github.com/miekg/dns"
)

func TestDNSOptimizationHTTPSResolverBoundariesAndCache(t *testing.T) {
	routingTestEnable(t)
	for _, name := range []string{"direct", "proxy", "explicit", "disabled", "IPv6-disabled", "force-AAAA-SOA"} {
		t.Run(name, func(t *testing.T) {
			config := selectionTestConfig()
			config.Fallback = nil
			config.DualStack = DualStackConfig{Enabled: true, AllowForceAAAA: true}
			leaf := newSelectionTestOutbound()
			leaf.delays[netip.MustParseAddr("192.0.2.1")] = 20 * time.Millisecond
			switch name {
			case "proxy":
				leaf.kind = C.Socks5
			case "explicit":
				config.Main[0].ProxyAdapter = newRoutingTestOutbound("fixed-DNS")
			case "disabled":
				config.SpeedCheck.Mode = []string{}
			case "IPv6-disabled":
				config.IPv6 = false
			case "force-AAAA-SOA":
				config.AnswerPolicy.ForceAAAASOA = true
			}
			rs := NewResolver(config)
			defer rs.Close()
			r := rs.Resolver
			client := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) {
				return speedCheckTestReply(t, q, `www.example. 60 IN HTTPS 1 target.example. alpn="h2,h3" port=8443 ipv4hint=192.0.2.1 ech=AAE= ipv6hint=2001:db8::1`), nil
			}}
			r.main = []dnsClient{client}
			query := new(D.Msg).SetQuestion("www.example.", D.TypeHTTPS)
			ctx := routingTestContext(leaf, routingTestOrigin())
			first := routingTestExchange(t, r, ctx, query)
			query.Id++
			cached := routingTestExchange(t, r, ctx, query)
			wantText := `www.example. 60 IN HTTPS 1 target.example. alpn="h2,h3" port=8443 ipv4hint=192.0.2.1 ech=AAE= ipv6hint=2001:db8::1`
			switch name {
			case "direct":
				wantText = `www.example. 60 IN HTTPS 1 target.example. alpn="h2,h3" port=8443 ech=AAE= ipv6hint=2001:db8::1`
			case "IPv6-disabled", "force-AAAA-SOA":
				wantText = `www.example. 60 IN HTTPS 1 target.example. alpn="h2,h3" port=8443 ipv4hint=192.0.2.1 ech=AAE=`
			}
			want := speedCheckTestRR(t, wantText).(*D.HTTPS)
			for _, reply := range []*D.Msg{first, cached} {
				record := reply.Answer[0].(*D.HTTPS)
				if record.Target != want.Target || record.Priority != want.Priority || !reflect.DeepEqual(record.Value, want.Value) || record.Hdr.Ttl == 0 || record.Hdr.Ttl > 60 {
					t.Fatalf("wrong HTTPS selection/cache result: %v, want %v", record, want)
				}
			}
			if client.count(D.TypeHTTPS) != 1 || client.count(0) != 1 {
				t.Fatalf("HTTPS started auxiliary DNS work or cache missed: %v", client.questions)
			}
			wantV4 := name == "direct" || name == "IPv6-disabled" || name == "force-AAAA-SOA"
			if leaf.probed("192.0.2.1") != wantV4 || leaf.probed("2001:db8::1") != (name == "direct") {
				t.Fatalf("HTTPS probes ignored the selected exit, disabled mode or IPv6 policy: %v", leaf.probes)
			}
		})
	}
}

func TestDNSOptimizationHTTPSResponseModesAndCacheImprovement(t *testing.T) {
	routingTestEnable(t)
	for _, mode := range []string{ResponseModeFirstPing, ResponseModeFastestIP, ResponseModeFastestResponse} {
		t.Run(mode, func(t *testing.T) {
			config := selectionTestConfig()
			config.Fallback = nil
			config.Main = append(config.Main, NameServer{Addr: "192.0.2.54:53"})
			config.SpeedCheck.ResponseMode = mode
			config.DualStack = DualStackConfig{Enabled: true, AllowForceAAAA: true}
			rs := NewResolver(config)
			defer rs.Close()
			r := rs.Resolver
			lateDNS, firstProbeStarted, finishFirstProbe := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var started sync.Once
			leaf := &responseModeTestOutbound{routingTestBase: &routingTestBase{name: "https-direct", kind: C.Direct}}
			leaf.probe = func(ctx context.Context, md *C.Metadata) error {
				if md.DstIP == netip.MustParseAddr("192.0.2.1") {
					started.Do(func() { close(firstProbeStarted) })
					select {
					case <-finishFirstProbe:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}
			first := &selectionTestClient{dnsClient: r.main[0], exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) {
				return speedCheckTestReply(t, q, `www.example. 60 IN HTTPS 1 first.example. alpn="h2" ipv4hint=192.0.2.1 ech=AAE=`), nil
			}}
			second := &selectionTestClient{dnsClient: r.main[1], exchange: func(ctx context.Context, q *D.Msg) (*D.Msg, error) {
				select {
				case <-lateDNS:
					return speedCheckTestReply(t, q, `www.example. 60 IN HTTPS 1 best.example. alpn="h3" ipv4hint=192.0.2.2 ech=AAI=`), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}}
			r.main = []dnsClient{first, second}
			query := new(D.Msg).SetQuestion("www.example.", D.TypeHTTPS)
			ctx := responseModePreparedContext(t, r, routingTestContext(leaf, routingTestOrigin()), query)
			replies := make(chan result, 1)
			go func() { message, err := r.ExchangeContext(ctx, query); replies <- result{message, err} }()
			assertEarly := func() {
				got := responseModeReceive(t, replies)
				if got.Error != nil || got.Msg.Answer[0].(*D.HTTPS).Target != "first.example." || minimalTTL(got.Msg.Answer) != dnsEarlyReplyTTL {
					t.Fatalf("HTTPS early response lost its binding or renewal TTL: %+v", got)
				}
			}
			if mode == ResponseModeFastestResponse {
				assertEarly()
			}
			responseModeReceive(t, firstProbeStarted)
			if mode != ResponseModeFastestResponse {
				select {
				case got := <-replies:
					t.Fatalf("HTTPS replied before a successful probe: %+v", got)
				default:
				}
			}
			time.Sleep(5 * time.Millisecond)
			close(finishFirstProbe)
			if mode == ResponseModeFirstPing {
				assertEarly()
			}
			if mode == ResponseModeFastestIP {
				select {
				case got := <-replies:
					t.Fatalf("HTTPS fastest-ip stopped before the remaining upstream: %+v", got)
				default:
				}
			}
			barrier := responseModeBarrier(r, ctx, query)
			close(lateDNS)
			final := responseModeReceive(t, barrier)
			if final.Err != nil || final.Val.Answer[0].(*D.HTTPS).Target != "best.example." {
				t.Fatalf("HTTPS final result did not improve: %+v", final)
			}
			if mode == ResponseModeFastestIP {
				got := responseModeReceive(t, replies)
				if got.Error != nil || got.Msg.Answer[0].(*D.HTTPS).Target != "best.example." {
					t.Fatalf("HTTPS full comparison chose the wrong binding: %+v", got)
				}
			}
			cached := routingTestExchange(t, r, ctx, query)
			if cached.Answer[0].(*D.HTTPS).Target != "best.example." || minimalTTL(cached.Answer) <= dnsEarlyReplyTTL ||
				first.count(D.TypeHTTPS) != 1 || second.count(D.TypeHTTPS) != 1 || first.count(0) != 1 || second.count(0) != 1 {
				t.Fatalf("HTTPS improvement lost cache lifetime or added DNS work: %v counts=%d/%d", cached, first.count(0), second.count(0))
			}
		})
	}
}

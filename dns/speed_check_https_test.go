package dns

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/dnsstats"
	D "github.com/miekg/dns"
)

func speedCheckHTTPSTestChecker(t *testing.T) *directSpeedChecker {
	t.Helper()
	checker, err := newDirectSpeedChecker(SpeedCheckConfig{
		Mode: []string{"tcp:443"}, Timeout: time.Second, Concurrency: 16, ResponseMode: ResponseModeFastestIP,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { checker.pool.Close(); checker.candidates.Close() })
	return checker
}

func TestSpeedCheckHTTPSSelectsHintsPerServiceRecord(t *testing.T) {
	query := new(D.Msg).SetQuestion("www.example.", D.TypeHTTPS)
	original := speedCheckTestReply(t, query,
		"www.example. 60 IN CNAME service.example.",
		`service.example. 60 IN HTTPS 1 a.example. mandatory=alpn,ipv4hint,ipv6hint,key65400 alpn="h2,h3" no-default-alpn port=8443 ipv4hint=192.0.2.1,192.0.2.2 ech=AAE= ipv6hint=2001:db8::1 key65400="opaque"`,
		`service.example. 60 IN HTTPS 2 b.example. alpn="h3" ipv4hint=192.0.2.3 ech=AAI= ipv6hint=2001:db8::2,2001:db8::3`,
		`service.example. 60 IN HTTPS 3 c.example. alpn="h2" ipv4hint=192.0.2.9 ipv6hint=2001:db8::9`,
		"unrelated.example. 60 IN HTTPS 1 . ipv4hint=192.0.2.99")
	original.Ns = []D.RR{speedCheckTestRR(t, "example. 60 IN NS ns.example.")}
	original.Extra = []D.RR{speedCheckTestRR(t, "ns.example. 60 IN A 192.0.2.100")}
	untouched := original.Copy()
	rtts := map[netip.Addr]time.Duration{
		netip.MustParseAddr("192.0.2.1"): 60 * time.Millisecond, netip.MustParseAddr("192.0.2.2"): 10 * time.Millisecond,
		netip.MustParseAddr("2001:db8::1"): 20 * time.Millisecond, netip.MustParseAddr("192.0.2.3"): 100 * time.Millisecond,
		netip.MustParseAddr("2001:db8::2"): 5 * time.Millisecond, netip.MustParseAddr("2001:db8::3"): 80 * time.Millisecond,
	}
	var mu sync.Mutex
	probed := make(map[netip.Addr]bool)
	s := speedCheckHTTPSTestChecker(t)
	s.probe = func(_ context.Context, ip netip.Addr, _ speedCheckMode) (time.Duration, error) {
		mu.Lock()
		probed[ip] = true
		mu.Unlock()
		if rtt, ok := rtts[ip]; ok {
			return rtt, nil
		}
		return 0, errors.New("unreachable")
	}
	got, _, err := s.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: original}}, query)
	if err != nil {
		t.Fatal(err)
	}
	want := speedCheckTestReply(t, query,
		"www.example. 60 IN CNAME service.example.",
		`service.example. 60 IN HTTPS 1 a.example. mandatory=alpn,ipv4hint,key65400 alpn="h2,h3" no-default-alpn port=8443 ipv4hint=192.0.2.2 ech=AAE= key65400="opaque"`,
		`service.example. 60 IN HTTPS 2 b.example. alpn="h3" ech=AAI= ipv6hint=2001:db8::2`,
		`service.example. 60 IN HTTPS 3 c.example. alpn="h2" ipv4hint=192.0.2.9 ipv6hint=2001:db8::9`,
		"unrelated.example. 60 IN HTTPS 1 . ipv4hint=192.0.2.99")
	want.Ns, want.Extra = original.Ns, original.Extra
	if !speedCheckTestEqual(got, want) || !speedCheckTestEqual(original, untouched) {
		t.Fatalf("service bindings were mixed, metadata changed, or source mutated:\ngot %v\nwant %v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(probed) != 8 || probed[netip.MustParseAddr("192.0.2.99")] || probed[netip.MustParseAddr("192.0.2.100")] {
		t.Fatalf("probed unrelated data or missed service hints: %v", probed)
	}
	wire, err := got.Pack()
	if err != nil {
		t.Fatal(err)
	}
	decoded := new(D.Msg)
	if err := decoded.Unpack(wire); err != nil || decoded.String() != got.String() {
		t.Fatalf("optimized HTTPS record cannot round-trip: %v %v", decoded, err)
	}
}

func TestSpeedCheckHTTPSKeepsWinningUpstreamBindingAndSources(t *testing.T) {
	query := new(D.Msg).SetQuestion("www.example.", D.TypeHTTPS)
	first := speedCheckTestReply(t, query,
		`www.example. 1 IN HTTPS 1 same.example. alpn="h2" ipv4hint=192.0.2.1 ech=AAE=`)
	second := speedCheckTestReply(t, query,
		`www.example. 600 IN HTTPS 1 same.example. alpn="h3" port=8443 ipv4hint=192.0.2.2 ech=AAI=`,
		`www.example. 600 IN HTTPS 2 backup.example. alpn="h2" ipv4hint=192.0.2.4,192.0.2.5 ech=AAM=`)
	ctx := withDNSAnswerSources(withDNSAnswerLifetimes(context.Background()))
	client := func(answer *D.Msg, address string) dnsClient {
		return &selectionTestClient{dnsClient: speedCheckTestClient{}, exchange: func(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
			message := answer.Copy()
			setDNSAnswerSources(ctx, message, []dnsstats.UpstreamSource{{Address: address, Outbound: "direct", OutboundType: "direct"}})
			return message, nil
		}}
	}
	s := speedCheckHTTPSTestChecker(t)
	s.probe = func(_ context.Context, ip netip.Addr, _ speedCheckMode) (time.Duration, error) {
		switch ip.String() {
		case "192.0.2.1":
			return 6 * time.Millisecond, nil
		case "192.0.2.2":
			return 5 * time.Millisecond, nil
		case "192.0.2.4":
			return 8 * time.Millisecond, nil
		default:
			return 20 * time.Millisecond, nil
		}
	}
	got, _, err := s.Exchange(ctx, []dnsClient{client(first, "udp://192.0.2.53:53"), client(second, "udp://192.0.2.54:53")}, query)
	want := speedCheckTestReply(t, query,
		`www.example. 600 IN HTTPS 1 same.example. alpn="h3" port=8443 ipv4hint=192.0.2.2 ech=AAI=`,
		`www.example. 600 IN HTTPS 2 backup.example. alpn="h2" ipv4hint=192.0.2.4 ech=AAM=`)
	if err != nil || !speedCheckTestEqual(got, want) {
		t.Fatalf("incompatible service profiles were merged: %v %v", got, err)
	}
	sources := dnsAnswerSourcesFor(ctx, got)
	if len(sources) != 1 || sources[0].Address != "udp://192.0.2.54:53" || dnsAnswerReceivedAt(ctx, got).IsZero() {
		t.Fatalf("HTTPS answer lost its contributing source or receipt clock: %v", sources)
	}
}

func TestSpeedCheckHTTPSPreservesAnswersWithoutMeasurements(t *testing.T) {
	for _, name := range []string{"no-hints", "alias", "failed", "budget", "late-dns", "invalid-addresses", "CNAME-cycle"} {
		t.Run(name, func(t *testing.T) {
			query := new(D.Msg).SetQuestion("www.example.", D.TypeHTTPS)
			records := []string{`www.example. 60 IN HTTPS 1 . alpn="h2" ipv4hint=192.0.2.1,192.0.2.2 ipv6hint=2001:db8::1`}
			switch name {
			case "no-hints":
				records = []string{`www.example. 60 IN HTTPS 1 target.example. alpn="h3" ech=AAE=`}
			case "alias":
				records = []string{"www.example. 60 IN HTTPS 0 target.example.", "target.example. 60 IN HTTPS 1 . ipv4hint=192.0.2.1"}
			case "invalid-addresses":
				records = []string{"www.example. 60 IN HTTPS 1 . ipv4hint=0.0.0.0,224.0.0.1,255.255.255.255 ipv6hint=::,ff02::1"}
			case "CNAME-cycle":
				records = append(records, "www.example. 60 IN CNAME loop.example.", "loop.example. 60 IN CNAME www.example.")
			}
			answer := speedCheckTestReply(t, query, records...)
			s := speedCheckHTTPSTestChecker(t)
			var probes atomic.Int32
			s.probe = func(ctx context.Context, _ netip.Addr, _ speedCheckMode) (time.Duration, error) {
				probes.Add(1)
				if name == "budget" {
					<-ctx.Done()
					return 0, ctx.Err()
				}
				return 0, errors.New("probe unavailable")
			}
			var delay time.Duration
			if name == "budget" || name == "late-dns" {
				s.timeout = 10 * time.Millisecond
			}
			if name == "late-dns" {
				delay = 30 * time.Millisecond
			}
			got, _, err := s.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: answer, delay: delay}}, query)
			if err != nil || !speedCheckTestEqual(got, answer) {
				t.Fatalf("lost a valid HTTPS fallback: %v %v", got, err)
			}
			if name != "failed" && name != "budget" && probes.Load() != 0 {
				t.Fatalf("unusable hints started probes: %d", probes.Load())
			}
		})
	}
}

func TestSpeedCheckHTTPSPreservesProtectedQueriesAndAnswers(t *testing.T) {
	for _, name := range []string{"DO", "CD", "query-TSIG", "AD", "RRSIG", "answer-TSIG", "truncated", "SVCB"} {
		t.Run(name, func(t *testing.T) {
			query := new(D.Msg).SetQuestion("www.example.", D.TypeHTTPS)
			switch name {
			case "DO":
				query.SetEdns0(1232, true)
			case "CD":
				query.CheckingDisabled = true
			case "query-TSIG":
				query.SetTsig("key.", D.HmacSHA256, 300, time.Now().Unix())
			case "SVCB":
				query.Question[0].Qtype = D.TypeSVCB
			}
			answer := speedCheckTestReply(t, query, "www.example. 60 IN HTTPS 1 . ipv4hint=192.0.2.1,192.0.2.2 ipv6hint=2001:db8::1")
			switch name {
			case "AD":
				answer.AuthenticatedData = true
			case "RRSIG":
				answer.Answer = append(answer.Answer, &D.RRSIG{Hdr: D.RR_Header{Name: "www.example.", Rrtype: D.TypeRRSIG, Class: D.ClassINET, Ttl: 60}})
			case "answer-TSIG":
				answer.SetTsig("key.", D.HmacSHA256, 300, time.Now().Unix())
			case "truncated":
				answer.Truncated = true
			}
			s := speedCheckHTTPSTestChecker(t)
			var probes atomic.Int32
			s.probe = func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
				probes.Add(1)
				return time.Millisecond, nil
			}
			got, _, err := s.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: answer}}, query)
			if err != nil || probes.Load() != 0 || !speedCheckTestEqual(got, answer) {
				t.Fatalf("protected HTTPS reply changed: probes=%d answer=%v error=%v", probes.Load(), got, err)
			}
		})
	}
}

func TestSpeedCheckHTTPSDoesNotVoteAcrossServiceBindings(t *testing.T) {
	query := new(D.Msg).SetQuestion("www.example.", D.TypeHTTPS)
	first := speedCheckTestReply(t, query, "www.example. 60 IN HTTPS 1 first.example. ipv4hint=192.0.2.1,192.0.2.2 ech=AAE=")
	repeated := speedCheckTestReply(t, query, "www.example. 60 IN HTTPS 1 other.example. ipv4hint=192.0.2.2 ech=AAI=")
	s := speedCheckHTTPSTestChecker(t)
	s.probe = func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
		return 0, errors.New("unavailable")
	}
	got, _, err := s.Exchange(context.Background(), []dnsClient{
		speedCheckTestClient{message: first}, speedCheckTestClient{message: repeated, delay: 10 * time.Millisecond},
		speedCheckTestClient{message: repeated, delay: 20 * time.Millisecond},
	}, query)
	if err != nil || !speedCheckTestEqual(got, first) {
		t.Fatalf("unmeasured hints were selected by votes or transplanted: %v %v", got, err)
	}
}

func TestSpeedCheckHTTPSMandatoryHintRemoval(t *testing.T) {
	query := new(D.Msg).SetQuestion("www.example.", D.TypeHTTPS)
	for _, preferred := range []string{"192.0.2.1", "2001:db8::1"} {
		t.Run(preferred, func(t *testing.T) {
			answer := speedCheckTestReply(t, query, "www.example. 60 IN HTTPS 1 . mandatory=ipv4hint,ipv6hint ipv4hint=192.0.2.1 ipv6hint=2001:db8::1")
			s := speedCheckHTTPSTestChecker(t)
			s.probe = func(_ context.Context, ip netip.Addr, _ speedCheckMode) (time.Duration, error) {
				if ip.String() == preferred {
					return time.Millisecond, nil
				}
				return time.Second, nil
			}
			got, _, err := s.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: answer}}, query)
			if err != nil {
				t.Fatal(err)
			}
			record := got.Answer[0].(*D.HTTPS)
			key := D.SVCB_IPV4HINT
			if preferred == "2001:db8::1" {
				key = D.SVCB_IPV6HINT
			}
			if !reflect.DeepEqual(record.Value[0].(*D.SVCBMandatory).Code, []D.SVCBKey{key}) || len(record.Value) != 2 {
				t.Fatalf("removed family is still mandatory: %v", record)
			}
		})
	}
}

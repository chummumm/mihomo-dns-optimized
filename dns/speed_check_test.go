package dns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/tunnel"

	D "github.com/miekg/dns"
)

type speedCheckTestClient struct {
	message *D.Msg
	delay   time.Duration
	err     error
}

func (c speedCheckTestClient) ExchangeContext(ctx context.Context, _ *D.Msg) (*D.Msg, error) {
	if c.delay > 0 {
		timer := time.NewTimer(c.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if c.message == nil {
		return nil, c.err
	}
	return c.message.Copy(), c.err
}

func (speedCheckTestClient) Address() string  { return "test://local" }
func (speedCheckTestClient) ResetConnection() {}

func speedCheckTestRR(t *testing.T, text string) D.RR {
	t.Helper()
	rr, err := D.NewRR(text)
	if err != nil {
		t.Fatal(err)
	}
	return rr
}

func speedCheckTestReply(t *testing.T, query *D.Msg, records ...string) *D.Msg {
	t.Helper()
	response := new(D.Msg).SetReply(query)
	for _, text := range records {
		response.Answer = append(response.Answer, speedCheckTestRR(t, text))
	}
	return response
}

func speedCheckTestEqual(a, b *D.Msg) bool {
	if a == nil || b == nil {
		return a == b
	}
	// dns.Msg.Copy normalizes nil RR sections to allocated empty sections.
	return reflect.DeepEqual(a.Copy(), b.Copy())
}

func speedCheckTestChecker(t *testing.T, timeout time.Duration, concurrency int) *directSpeedChecker {
	t.Helper()
	s, err := newDirectSpeedChecker(SpeedCheckConfig{Mode: []string{"tcp:443", "tcp:80", "ping"}, Timeout: timeout, Concurrency: concurrency})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.pool.Close()
		s.candidates.Close()
	})
	return s
}

func TestSpeedCheckConfiguration(t *testing.T) {
	for _, config := range []SpeedCheckConfig{{}, {Mode: []string{"none"}}} {
		s, err := newDirectSpeedChecker(config)
		if err != nil || s != nil {
			t.Fatalf("disabled checker: %v, %v", s, err)
		}
	}
	for _, config := range []SpeedCheckConfig{
		{Mode: []string{"none", "ping"}}, {Mode: []string{"udp:53"}}, {Mode: []string{"tcp:0"}},
		{Mode: []string{"tcp:65536"}}, {Mode: []string{"tcp:abc"}}, {Mode: []string{""}},
		{Timeout: -time.Second}, {Concurrency: -1}, {Concurrency: maxSpeedCheckConcurrency + 1},
	} {
		if err := ValidateSpeedCheckConfig(config); err == nil {
			t.Fatalf("accepted invalid configuration: %+v", config)
		}
	}
	s, err := newDirectSpeedChecker(SpeedCheckConfig{Mode: []string{"tcp:443", "tcp:443", "tcp:80", "ping"}})
	if err != nil || len(s.modes) != 3 || s.timeout <= 0 || s.concurrency <= 0 {
		t.Fatalf("defaults/deduplication: %+v, %v", s, err)
	}
}

func TestSpeedCheckCollectsLaterUpstreamAndPreservesCNAME(t *testing.T) {
	query := new(D.Msg).SetQuestion("www.example.", D.TypeA)
	first := speedCheckTestReply(t, query,
		"www.example. 60 IN CNAME first.example.", "first.example. 60 IN A 192.0.2.1")
	second := speedCheckTestReply(t, query,
		"www.example. 45 IN CNAME cdn.example.", "cdn.example. 45 IN CNAME final.example.",
		"final.example. 40 IN A 192.0.2.2", "final.example. 40 IN A 192.0.2.3",
		"final.example. 30 IN HTTPS 1 . alpn=\"h2\"", "unrelated.example. 20 IN A 192.0.2.250")
	second.Ns = []D.RR{speedCheckTestRR(t, "example. 60 IN NS ns.example.")}
	second.Extra = []D.RR{speedCheckTestRR(t, "ns.example. 60 IN A 192.0.2.53")}
	before := second.Copy()
	s := speedCheckTestChecker(t, time.Second, 4)
	var probes atomic.Int32
	s.probe = func(_ context.Context, ip netip.Addr, _ speedCheckMode) (time.Duration, error) {
		probes.Add(1)
		switch ip.String() {
		case "192.0.2.1":
			return 30 * time.Millisecond, nil
		case "192.0.2.2":
			return 10 * time.Millisecond, nil
		case "192.0.2.3":
			return time.Millisecond, nil
		default:
			return 0, errors.New("unrelated records must never be probed")
		}
	}
	got, cache, err := s.Exchange(context.Background(), []dnsClient{
		speedCheckTestClient{message: first}, speedCheckTestClient{message: second, delay: 15 * time.Millisecond},
	}, query)
	if err != nil || !cache {
		t.Fatalf("exchange: %v, cache=%v", err, cache)
	}
	want := second.Copy()
	want.Answer = append(want.Answer[:2], want.Answer[3:]...)
	if !speedCheckTestEqual(got, want) {
		t.Fatalf("did not preserve winning response's chain/other records:\ngot %s\nwant %s", got, want)
	}
	if probes.Load() != 3 || !speedCheckTestEqual(second, before) {
		t.Fatalf("unexpected probe count or mutated upstream: probes=%d", probes.Load())
	}
}

func TestSpeedCheckAAAAAndDuplicateCandidates(t *testing.T) {
	query := new(D.Msg).SetQuestion("example.", D.TypeAAAA)
	response := speedCheckTestReply(t, query,
		"example. 60 IN AAAA 2001:db8::1", "example. 60 IN AAAA 2001:db8::2")
	s := speedCheckTestChecker(t, time.Second, 2)
	var calls atomic.Int32
	s.probe = func(_ context.Context, ip netip.Addr, _ speedCheckMode) (time.Duration, error) {
		calls.Add(1)
		if ip == netip.MustParseAddr("2001:db8::2") {
			return time.Millisecond, nil
		}
		return 2 * time.Millisecond, nil
	}
	got, _, err := s.Exchange(context.Background(), []dnsClient{
		speedCheckTestClient{message: response}, speedCheckTestClient{message: response},
	}, query)
	if err != nil || len(got.Answer) != 1 || got.Answer[0].(*D.AAAA).AAAA.String() != "2001:db8::2" || calls.Load() != 2 {
		t.Fatalf("AAAA/deduplication: answer=%v calls=%d error=%v", got, calls.Load(), err)
	}
}

func TestSpeedCheckUnreachablePreservesPositiveAnswer(t *testing.T) {
	query := new(D.Msg).SetQuestion("example.", D.TypeA)
	negative := new(D.Msg).SetRcode(query, D.RcodeNameError)
	positive := speedCheckTestReply(t, query, "example. 60 IN A 192.0.2.1", "example. 60 IN A 192.0.2.2")
	s := speedCheckTestChecker(t, time.Second, 2)
	s.probe = func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
		return 0, errors.New("unreachable or ICMP permission denied")
	}
	got, _, err := s.Exchange(context.Background(), []dnsClient{
		speedCheckTestClient{message: negative}, speedCheckTestClient{message: positive, delay: 10 * time.Millisecond},
	}, query)
	if err != nil || !speedCheckTestEqual(got, positive) {
		t.Fatalf("probe failure lost the positive DNS response: %v, %v", got, err)
	}
}

func TestSpeedCheckOptimizationDeadlinePreservesAvailableAnswer(t *testing.T) {
	query := new(D.Msg).SetQuestion("example.", D.TypeA)
	positive := speedCheckTestReply(t, query, "example. 60 IN A 192.0.2.1")
	s := speedCheckTestChecker(t, 40*time.Millisecond, 2)
	s.probe = func(ctx context.Context, _ netip.Addr, _ speedCheckMode) (time.Duration, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	start := time.Now()
	got, _, err := s.Exchange(context.Background(), []dnsClient{
		speedCheckTestClient{message: positive}, speedCheckTestClient{message: positive, delay: time.Hour},
	}, query)
	if err != nil || !speedCheckTestEqual(got, positive) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("deadline did not preserve available answer: %v, %v, elapsed=%v", got, err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.Exchange(ctx, []dnsClient{speedCheckTestClient{delay: time.Hour}}, query); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled exchange: %v", err)
	}
}

func TestSpeedCheckConcurrencySharedAcrossQueries(t *testing.T) {
	query := new(D.Msg).SetQuestion("example.", D.TypeA)
	response := speedCheckTestReply(t, query, "example. 60 IN A 192.0.2.1", "example. 60 IN A 192.0.2.2")
	s := speedCheckTestChecker(t, time.Second, 2)
	var active, maximum, calls atomic.Int32
	s.probe = func(ctx context.Context, _ netip.Addr, _ speedCheckMode) (time.Duration, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
		calls.Add(1)
		timer := time.NewTimer(2 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-timer.C:
			return time.Millisecond, nil
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: response}}, query)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maximum.Load() > 2 || calls.Load() == 0 || calls.Load() > 16 {
		t.Fatalf("shared probe limit broken: maximum=%d calls=%d", maximum.Load(), calls.Load())
	}
}

func TestSpeedCheckFallbackModeGetsTimeAfterBlackhole(t *testing.T) {
	s, err := newDirectSpeedChecker(SpeedCheckConfig{Mode: []string{"tcp:443", "tcp:80"}, Timeout: 80 * time.Millisecond, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	var called []uint16
	rtt, err := s.checkIP(ctx, netip.MustParseAddr("192.0.2.1"), func(ctx context.Context, _ netip.Addr, mode speedCheckMode) (time.Duration, error) {
		called = append(called, mode.port)
		if mode.port == 443 {
			<-ctx.Done()
			return 0, ctx.Err()
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return time.Millisecond, nil
	})
	if err != nil || rtt != time.Millisecond || !reflect.DeepEqual(called, []uint16{443, 80}) {
		t.Fatalf("fallback mode was starved: %v %v %v", called, rtt, err)
	}
}

func TestSpeedCheckPreservesDNSSECAndNonAddressQuestions(t *testing.T) {
	for _, name := range []string{"DO", "CD", "AD", "TSIG", "HTTPS"} {
		t.Run(name, func(t *testing.T) {
			query := new(D.Msg).SetQuestion("example.", D.TypeA)
			switch name {
			case "DO":
				query.SetEdns0(1232, true)
			case "CD":
				query.CheckingDisabled = true
			case "AD":
				query.AuthenticatedData = true
			case "TSIG":
				query.SetTsig("key.", D.HmacSHA256, 300, time.Now().Unix())
			case "HTTPS":
				query.SetQuestion("example.", D.TypeHTTPS)
			}
			response := speedCheckTestReply(t, query, "example. 60 IN A 192.0.2.1", "example. 60 IN A 192.0.2.2")
			s := speedCheckTestChecker(t, time.Second, 1)
			var calls atomic.Int32
			s.probe = func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
				calls.Add(1)
				return time.Millisecond, nil
			}
			got, _, err := s.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: response}}, query)
			if err != nil || calls.Load() != 0 || !speedCheckTestEqual(got, response) {
				t.Fatalf("protected/non-address request changed: calls=%d answer=%v error=%v", calls.Load(), got, err)
			}
		})
	}
	for _, name := range []string{"AD", "RRSIG", "TSIG", "SIG0"} {
		t.Run("response-"+name, func(t *testing.T) {
			query := new(D.Msg).SetQuestion("example.", D.TypeA)
			response := speedCheckTestReply(t, query, "example. 60 IN A 192.0.2.1", "example. 60 IN A 192.0.2.2")
			switch name {
			case "AD":
				response.AuthenticatedData = true
			case "RRSIG":
				response.Answer = append(response.Answer, &D.RRSIG{Hdr: D.RR_Header{Name: "example.", Rrtype: D.TypeRRSIG, Class: D.ClassINET, Ttl: 60}})
			case "TSIG":
				response.SetTsig("key.", D.HmacSHA256, 300, time.Now().Unix())
			case "SIG0":
				response.Extra = append(response.Extra, &D.SIG{RRSIG: D.RRSIG{Hdr: D.RR_Header{Name: "example.", Rrtype: D.TypeSIG}}})
			}
			s := speedCheckTestChecker(t, time.Second, 1)
			var calls atomic.Int32
			s.probe = func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
				calls.Add(1)
				return time.Millisecond, nil
			}
			got, _, err := s.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: response}}, query)
			if err != nil || calls.Load() != 0 || !speedCheckTestEqual(got, response) {
				t.Fatalf("protected response changed: calls=%d answer=%v error=%v", calls.Load(), got, err)
			}
		})
	}
}

func TestSpeedCheckProxyPlanNeverProbes(t *testing.T) {
	query := new(D.Msg).SetQuestion("example.", D.TypeA)
	proxy := newRoutingTestOutbound("proxy")
	proxy.kind.Store(int32(C.Socks5))
	ctx := icontext.WithDNSFixedOutbound(context.Background(), proxy)
	plan, err := tunnel.PrepareDNSRouting(ctx, "example", &C.Metadata{NetWork: C.UDP, Type: C.INNER})
	if err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, dnsQueryRouteKey{}, &dnsQueryRoute{plan: plan})
	response := speedCheckTestReply(t, query, "example. 60 IN A 192.0.2.1", "example. 60 IN A 192.0.2.2")
	s := speedCheckTestChecker(t, time.Second, 1)
	var calls atomic.Int32
	s.probe = func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
		calls.Add(1)
		return time.Millisecond, nil
	}
	got, _, err := s.Exchange(ctx, []dnsClient{speedCheckTestClient{message: response}}, query)
	if err != nil || calls.Load() != 0 || !speedCheckTestEqual(got, response) {
		t.Fatalf("proxy query was speed checked: calls=%d answer=%v error=%v", calls.Load(), got, err)
	}
}

func TestSpeedCheckMalformedChainAndCandidateBound(t *testing.T) {
	query := new(D.Msg).SetQuestion("example.", D.TypeA)
	cycle := speedCheckTestReply(t, query,
		"example. 60 IN CNAME alias.example.", "alias.example. 60 IN CNAME example.", "alias.example. 60 IN A 192.0.2.1")
	if got := speedCheckCandidates(cycle, query.Question[0]); len(got) != 0 {
		t.Fatalf("CNAME cycle produced candidates: %v", got)
	}
	ambiguous := speedCheckTestReply(t, query,
		"example. 60 IN CNAME first.example.", "example. 60 IN CNAME second.example.", "first.example. 60 IN A 192.0.2.1")
	if got := speedCheckCandidates(ambiguous, query.Question[0]); len(got) != 0 {
		t.Fatalf("ambiguous CNAME produced candidates: %v", got)
	}
	response := speedCheckTestReply(t, query)
	for i := 0; i < maxSpeedCheckCandidates+20; i++ {
		response.Answer = append(response.Answer, &D.A{Hdr: D.RR_Header{Name: "example.", Rrtype: D.TypeA, Class: D.ClassINET, Ttl: 60}, A: net.IPv4(198, 18, byte(i/256), byte(i%256))})
	}
	s := speedCheckTestChecker(t, time.Second, maxSpeedCheckCandidates)
	var calls atomic.Int32
	s.probe = func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
		calls.Add(1)
		return time.Millisecond, nil
	}
	if _, _, err := s.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: response}}, query); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != maxSpeedCheckCandidates {
		t.Fatalf("candidate cap: %d", calls.Load())
	}
}

func TestSpeedCheckDirectTCPProbeUsesLocalIP(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			err = conn.Close()
		}
		accepted <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	rtt, err := probeDirectIP(ctx, netip.MustParseAddr("127.0.0.1"), speedCheckMode{port: port})
	if err != nil || rtt < 0 {
		t.Fatalf("local TCP probe: %v %v", rtt, err)
	}
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("local probe did not connect")
	}
}

func TestSpeedCheckICMPLoopback(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "::1"} {
		t.Run(address, func(t *testing.T) {
			ip := netip.MustParseAddr(address)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			rtt, err := pingDirectIP(ctx, ip)
			if err != nil {
				for _, unsupported := range []error{
					syscall.EPERM, syscall.EACCES, syscall.EAFNOSUPPORT,
					syscall.EPROTONOSUPPORT, syscall.EOPNOTSUPP, syscall.ENOSYS,
				} {
					if errors.Is(err, unsupported) {
						t.Skipf("ICMP loopback unavailable due to explicit capability/platform error: %v", err)
					}
				}
				if ip.Is6() && errors.Is(err, syscall.EADDRNOTAVAIL) {
					t.Skipf("IPv6 loopback unavailable on this host: %v", err)
				}
				t.Fatalf("ICMP loopback failed (timeouts and malformed replies are not skipped): %v", err)
			}
			if rtt <= 0 {
				t.Fatalf("ICMP loopback returned invalid RTT: %v", rtt)
			}
			t.Logf("ICMP echo/reply succeeded for %s: RTT %v", address, rtt)
		})
	}
}

func TestSpeedCheckPerQueryProbeOverride(t *testing.T) {
	query := new(D.Msg).SetQuestion("example.", D.TypeA)
	response := speedCheckTestReply(t, query, "example. 60 IN A 192.0.2.1")
	s := speedCheckTestChecker(t, time.Second, 1)
	var defaults, custom atomic.Int32
	s.probe = func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
		defaults.Add(1)
		return 0, errors.New("wrong interface")
	}
	_, _, err := s.ExchangeWithProbe(context.Background(), []dnsClient{speedCheckTestClient{message: response}}, query,
		func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error) {
			custom.Add(1)
			return time.Millisecond, nil
		})
	if err != nil || defaults.Load() != 0 || custom.Load() != 1 {
		t.Fatalf("selected adapter's probe was not used: default=%d custom=%d err=%v", defaults.Load(), custom.Load(), err)
	}
}

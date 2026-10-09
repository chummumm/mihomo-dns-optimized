package dns

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/metacubex/mihomo/component/dialer"
	C "github.com/metacubex/mihomo/constant"

	D "github.com/miekg/dns"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	defaultSpeedCheckTimeout     = time.Second
	defaultSpeedCheckConcurrency = 16
	maxSpeedCheckConcurrency     = 256
	maxSpeedCheckCandidates      = 256
)

// SpeedCheckConfig controls optional DIRECT destination-IP selection. A zero
// value disables it. Timeout is one budget for upstream collection and probes,
// not a separate timeout for every address. Concurrency limits active probes
// across queries sharing a checker.
type SpeedCheckConfig struct {
	Mode        []string
	Timeout     time.Duration
	Concurrency int
}

type speedCheckMode struct {
	port uint16 // zero denotes ICMP echo
}

type speedCheckProbe func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error)

type directSpeedChecker struct {
	modes       []speedCheckMode
	timeout     time.Duration
	concurrency int
	slots       chan struct{}
	probe       speedCheckProbe
	candidates  *dnsCandidateCache
	pool        *dnsProbePool
}

// ValidateSpeedCheckConfig also validates disabled configurations, so a typo
// cannot stay hidden until speed checking is enabled later.
func ValidateSpeedCheckConfig(config SpeedCheckConfig) error {
	_, err := newDirectSpeedChecker(config)
	return err
}

func newDirectSpeedChecker(config SpeedCheckConfig) (*directSpeedChecker, error) {
	if config.Timeout < 0 {
		return nil, errors.New("dns.speed-check-timeout must not be negative")
	}
	if config.Concurrency < 0 || config.Concurrency > maxSpeedCheckConcurrency {
		return nil, fmt.Errorf("dns.speed-check-concurrency must be between 1 and %d (or 0 for default)", maxSpeedCheckConcurrency)
	}
	var modes []speedCheckMode
	seen := make(map[string]bool)
	for _, value := range config.Mode {
		value = strings.TrimSpace(strings.ToLower(value))
		if value == "none" {
			if len(config.Mode) != 1 {
				return nil, errors.New("dns.speed-check-mode: none must be used alone")
			}
			return nil, nil
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		switch {
		case value == "ping":
			modes = append(modes, speedCheckMode{})
		case strings.HasPrefix(value, "tcp:"):
			port, err := strconv.ParseUint(strings.TrimPrefix(value, "tcp:"), 10, 16)
			if err != nil || port == 0 {
				return nil, fmt.Errorf("dns.speed-check-mode: invalid TCP port in %q", value)
			}
			modes = append(modes, speedCheckMode{port: uint16(port)})
		default:
			return nil, fmt.Errorf("dns.speed-check-mode: unsupported mode %q", value)
		}
	}
	if len(modes) == 0 {
		return nil, nil
	}
	if config.Timeout == 0 {
		config.Timeout = defaultSpeedCheckTimeout
	}
	if config.Concurrency == 0 {
		config.Concurrency = defaultSpeedCheckConcurrency
	}
	return &directSpeedChecker{
		modes: modes, timeout: config.Timeout, concurrency: config.Concurrency,
		slots: make(chan struct{}, config.Concurrency), probe: probeDirectIP,
		pool:       newDNSProbePool(config.Concurrency, maxSpeedCheckCandidates, config.Timeout),
		candidates: newDNSCandidateCache(),
	}, nil
}

type speedCheckCandidate struct {
	ip       netip.Addr
	owner    string
	response *D.Msg
}

type speedCheckResult struct {
	candidate speedCheckCandidate
	rtt       time.Duration
	err       error
}

// Exchange must only be called with an upstream pool selected for DIRECT. An
// existing non-DIRECT routing plan is also rejected defensively. Different
// upstream CNAME chains and RRsets are never merged: the winning IP selects one
// intact response, whose unsigned terminal address RRset may then be narrowed.
func (s *directSpeedChecker) Exchange(ctx context.Context, clients []dnsClient, query *D.Msg) (*D.Msg, bool, error) {
	var probe speedCheckProbe
	if s != nil {
		probe = s.probe
	}
	return s.ExchangeWithProbe(ctx, clients, query, probe)
}

// ExchangeWithProbe accepts a per-query probe bound to the already selected
// DIRECT adapter, including that adapter's interface and routing mark. The
// callback must honor its context and return an error for unsupported modes
// (for example ping when it cannot inherit the adapter's socket policy).
func (s *directSpeedChecker) ExchangeWithProbe(ctx context.Context, clients []dnsClient, query *D.Msg, probe speedCheckProbe) (*D.Msg, bool, error) {
	if query == nil || len(query.Question) == 0 {
		return nil, true, errors.New("DNS speed check requires a question")
	}
	if s == nil || probe == nil || !speedCheckQuestion(query) {
		return batchExchange(ctx, clients, query)
	}
	if route := queryRoute(ctx); route != nil {
		if route.plan == nil || (route.plan.Type() != C.Direct && route.plan.Type() != C.Compatible) {
			return batchExchange(ctx, clients, query)
		}
	}
	for _, client := range clients {
		if _, ok := client.(rcodeClient); ok {
			response, err := client.ExchangeContext(ctx, query)
			return response, false, err
		}
	}
	if len(clients) == 0 {
		return nil, true, errors.New("all DNS requests failed: no upstreams")
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	type upstreamResult struct {
		message *D.Msg
		err     error
	}
	upstreams := make(chan upstreamResult, s.concurrency)
	workerCount := minSpeedCheck(s.concurrency, len(clients))
	for worker := 0; worker < workerCount; worker++ {
		go func(worker int) {
			for i := worker; i < len(clients); i += workerCount {
				if ctx.Err() != nil {
					return
				}
				message, err := clients[i].ExchangeContext(ctx, query.Copy())
				select {
				case upstreams <- upstreamResult{message, err}:
				case <-ctx.Done():
					return
				}
			}
		}(worker)
	}
	probes := make(chan speedCheckResult, maxSpeedCheckCandidates)
	var tickets []*dnsProbeTicket
	defer func() {
		for _, ticket := range tickets {
			ticket.Release()
		}
	}()

	var firstResponse *D.Msg
	var firstAddressResponse *D.Msg
	var firstError error
	var best *speedCheckResult
	seen := make(map[netip.Addr]bool)
	remaining, pending := len(clients), 0
	finish := func() (*D.Msg, bool, error) {
		if best != nil {
			return speedCheckAnswer(best.candidate, query.Question[0].Qtype), true, nil
		}
		if firstAddressResponse != nil {
			return firstAddressResponse, true, nil
		}
		if firstResponse != nil {
			return firstResponse, true, nil
		}
		if firstError != nil {
			return nil, true, fmt.Errorf("all DNS requests failed: %w", firstError)
		}
		if err := ctx.Err(); err != nil {
			return nil, true, err
		}
		return nil, true, errors.New("all DNS requests failed")
	}
	for remaining > 0 || pending > 0 {
		select {
		case <-ctx.Done():
			return finish()
		case result := <-upstreams:
			remaining--
			if result.err == nil && !speedCheckResponseMatches(query, result.message) {
				result.err = errors.New("invalid DNS upstream response")
			}
			if result.err == nil && (result.message.Rcode == D.RcodeServerFailure || result.message.Rcode == D.RcodeRefused) {
				result.err = fmt.Errorf("server failure: %s", D.RcodeToString[result.message.Rcode])
			}
			if result.err != nil {
				if firstError == nil {
					firstError = result.err
				}
				continue
			}
			if firstResponse == nil {
				firstResponse = result.message
			}
			if speedCheckProtected(result.message) {
				return result.message, true, nil
			}
			candidates := speedCheckCandidates(result.message, query.Question[0])
			if firstAddressResponse == nil && len(candidates) != 0 {
				firstAddressResponse = result.message
			}
			for _, candidate := range candidates {
				if seen[candidate.ip] || len(seen) >= maxSpeedCheckCandidates {
					continue
				}
				seen[candidate.ip] = true
				pending++
				candidate := candidate
				ticket, err := s.pool.submit(ctx, dnsProbeScope(ctx, candidate.ip),
					func(workCtx context.Context) (time.Duration, error) { return s.checkIP(workCtx, candidate.ip, probe) },
					func(rtt time.Duration, err error) { probes <- speedCheckResult{candidate, rtt, err} })
				if err != nil {
					probes <- speedCheckResult{candidate, 0, err}
				}
				if ticket != nil {
					tickets = append(tickets, ticket)
				}
			}
		case result := <-probes:
			pending--
			// Every waiter, including a reused result, owns its family
			// measurements. Recording only inside the actual socket probe
			// would lose measurements for coalesced or cached callers.
			if measurements, ok := ctx.Value(dnsProbeMeasurementsKey{}).(*dualStackMeasurements); ok {
				measurements.observe(result.candidate.ip, result.rtt, result.err)
			}
			if result.err == nil && result.rtt >= 0 && (best == nil || result.rtt < best.rtt) {
				best = &result
			}
		}
	}
	return finish()
}

func speedCheckQuestion(message *D.Msg) bool {
	if message.CheckingDisabled || speedCheckProtected(message) {
		return false
	}
	if opt := message.IsEdns0(); opt != nil && opt.Do() {
		return false
	}
	return len(message.Question) == 1 && message.Opcode == D.OpcodeQuery &&
		message.Question[0].Qclass == D.ClassINET &&
		(message.Question[0].Qtype == D.TypeA || message.Question[0].Qtype == D.TypeAAAA)
}

func speedCheckResponseMatches(query, response *D.Msg) bool {
	if response == nil || !response.Response || response.Opcode != query.Opcode || len(response.Question) != 1 {
		return false
	}
	q, r := query.Question[0], response.Question[0]
	return strings.EqualFold(q.Name, r.Name) && q.Qtype == r.Qtype && q.Qclass == r.Qclass
}

func speedCheckCandidates(message *D.Msg, question D.Question) []speedCheckCandidate {
	if message.Rcode != D.RcodeSuccess || message.Truncated {
		return nil
	}
	cnames := make(map[string]string)
	for _, rr := range message.Answer {
		if cname, ok := rr.(*D.CNAME); ok && cname.Hdr.Class == D.ClassINET {
			owner, target := strings.ToLower(cname.Hdr.Name), strings.ToLower(cname.Target)
			if old, exists := cnames[owner]; exists && old != target {
				return nil
			}
			cnames[owner] = target
		}
	}
	owner := strings.ToLower(question.Name)
	visited := make(map[string]bool)
	for cnames[owner] != "" {
		if visited[owner] {
			return nil
		}
		visited[owner] = true
		owner = cnames[owner]
	}
	var result []speedCheckCandidate
	for _, rr := range message.Answer {
		if rr.Header().Class != D.ClassINET || rr.Header().Rrtype != question.Qtype || !strings.EqualFold(rr.Header().Name, owner) {
			continue
		}
		ip, ok := speedCheckAddress(rr)
		if ok && !ip.IsUnspecified() && !ip.IsMulticast() && ip != netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
			result = append(result, speedCheckCandidate{ip, owner, message})
		}
	}
	return result
}

func speedCheckAddress(rr D.RR) (netip.Addr, bool) {
	var address net.IP
	switch record := rr.(type) {
	case *D.A:
		address = record.A
	case *D.AAAA:
		address = record.AAAA
	default:
		return netip.Addr{}, false
	}
	ip, ok := netip.AddrFromSlice(address)
	return ip.Unmap(), ok
}

func speedCheckAnswer(candidate speedCheckCandidate, qtype uint16) *D.Msg {
	message := candidate.response.Copy()
	// Do not rewrite authenticated/signed RRsets, including TSIG and SIG(0).
	if speedCheckProtected(message) {
		return message
	}
	answer := message.Answer[:0]
	for _, rr := range message.Answer {
		if rr.Header().Class == D.ClassINET && rr.Header().Rrtype == qtype && strings.EqualFold(rr.Header().Name, candidate.owner) {
			if ip, ok := speedCheckAddress(rr); ok && ip != candidate.ip {
				continue
			}
		}
		answer = append(answer, rr)
	}
	message.Answer = answer
	return message
}

func speedCheckProtected(message *D.Msg) bool {
	if message.AuthenticatedData || message.IsTsig() != nil {
		return true
	}
	for _, section := range [][]D.RR{message.Answer, message.Ns, message.Extra} {
		for _, rr := range section {
			if rr.Header().Rrtype == D.TypeRRSIG || rr.Header().Rrtype == D.TypeSIG {
				return true
			}
		}
	}
	return false
}

func (s *directSpeedChecker) checkIP(ctx context.Context, ip netip.Addr, probe speedCheckProbe) (time.Duration, error) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	var lastError error
	for i, mode := range s.modes {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		deadline, _ := ctx.Deadline()
		// Reserve a fair share for fallback modes: an unresponsive TCP port
		// must not consume the entire budget and prevent TCP:80 or ping.
		modeCtx, cancel := context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(s.modes)-i))
		rtt, err := probe(modeCtx, ip, mode)
		cancel()
		if err == nil {
			return rtt, nil
		}
		lastError = err
	}
	return 0, lastError
}

func probeDirectIP(ctx context.Context, ip netip.Addr, mode speedCheckMode) (time.Duration, error) {
	if mode.port == 0 {
		return pingDirectIP(ctx, ip)
	}
	start := time.Now()
	// Numeric addresses avoid DNS recursion. The low-level dialer preserves
	// Mihomo's socket/interface policy without entering tunnel rule matching
	// or adding synthetic connection entries to the traffic panel.
	conn, err := dialer.DialContext(ctx, "tcp", netip.AddrPortFrom(ip, mode.port).String())
	if err != nil {
		return 0, err
	}
	rtt := time.Since(start)
	_ = conn.Close()
	return rtt, nil
}

func pingDirectIP(ctx context.Context, ip netip.Addr, options ...dialer.Option) (time.Duration, error) {
	network, local, protocol := "ip4:icmp", "0.0.0.0", 1
	var requestType icmp.Type = ipv4.ICMPTypeEcho
	var replyType icmp.Type = ipv4.ICMPTypeEchoReply
	if ip.Is6() {
		network, local, protocol = "ip6:ipv6-icmp", "::", 58
		requestType, replyType = ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply
	}
	// Permission or platform failures simply fail this optional probe. No
	// subprocess, privilege escalation, or fallback through a proxy is used.
	listener := net.ListenConfig{Control: dialer.ICMPControlWithOptions(ip, options...)}
	conn, err := listener.ListenPacket(ctx, network, local)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return 0, err
		}
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	var nonce [18]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return 0, err
	}
	id := int(binary.BigEndian.Uint16(nonce[:2]))
	echo := &icmp.Message{Type: requestType, Body: &icmp.Echo{ID: id, Seq: 1, Data: nonce[2:]}}
	wire, err := echo.Marshal(nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	if _, err := conn.WriteTo(wire, &net.IPAddr{IP: net.IP(ip.AsSlice()), Zone: ip.Zone()}); err != nil {
		return 0, err
	}
	buffer := make([]byte, 1500)
	for {
		n, source, err := conn.ReadFrom(buffer)
		if err != nil {
			return 0, err
		}
		sourceIP, ok := source.(*net.IPAddr)
		if !ok || !sourceIP.IP.Equal(net.IP(ip.AsSlice())) {
			continue
		}
		message, err := icmp.ParseMessage(protocol, buffer[:n])
		if err != nil || message.Type != replyType || message.Code != 0 {
			continue
		}
		body, ok := message.Body.(*icmp.Echo)
		if ok && body.ID == id && body.Seq == 1 && bytes.Equal(body.Data, nonce[2:]) {
			return time.Since(start), nil
		}
	}
}

func minSpeedCheck(a, b int) int {
	if a < b {
		return a
	}
	return b
}

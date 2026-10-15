package dns

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/component/dialer"
	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/tunnel"

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
	maxSpeedCheckModes           = 4
	speedCheckStageInterval      = 100 * time.Millisecond
)

// DefaultMaxReplyIPNum matches SmartDNS's default upper bound for optimized
// address answers. It does not require padding a reply with slower addresses.
const DefaultMaxReplyIPNum = 8

// SpeedCheckConfig controls optional DIRECT destination-IP selection. A nil
// Mode selects automatic probes; an explicitly empty Mode disables
// them. Timeout bounds selection work; an upstream which has not answered yet
// retains the normal DNS deadline after that budget ends.
// Concurrency limits active IP probes across queries sharing a checker.
// MaxReplyIPNum bounds the retained fast addresses, with zero selecting eight.
type SpeedCheckConfig struct {
	Mode          []string
	Timeout       time.Duration
	Concurrency   int
	ResponseMode  string
	MaxReplyIPNum int
}

type speedCheckMode struct {
	port    uint16 // zero denotes ICMP echo
	tcpMode tcpProbeMode
}

func (m speedCheckMode) String() string {
	if m.port == 0 {
		return "ping"
	}
	switch m.tcpMode {
	case tcpProbeSYN:
		return "tcp-syn:" + strconv.Itoa(int(m.port))
	case tcpProbeAuto:
		return "auto:" + strconv.Itoa(int(m.port))
	}
	return "tcp:" + strconv.Itoa(int(m.port))
}

type speedCheckProbe func(context.Context, netip.Addr, speedCheckMode) (time.Duration, error)

type directSpeedChecker struct {
	modes         []speedCheckMode
	responseMode  string
	maxReplyIPNum int
	timeout       time.Duration
	concurrency   int
	slots         chan struct{}
	probe         speedCheckProbe
	candidates    *dnsCandidateCache
	pool          *dnsProbePool
	synWarning    sync.Once
}

// ValidateSpeedCheckConfig also validates disabled configurations, so a typo
// cannot stay hidden until speed checking is enabled later.
func ValidateSpeedCheckConfig(config SpeedCheckConfig) error {
	_, err := newDirectSpeedChecker(config)
	return err
}

func newDirectSpeedChecker(config SpeedCheckConfig) (*directSpeedChecker, error) {
	if err := ValidateResponseMode(config.ResponseMode); err != nil {
		return nil, err
	}
	if config.Timeout < 0 {
		return nil, errors.New("dns.speed-check-timeout must not be negative")
	}
	if config.Concurrency < 0 || config.Concurrency > maxSpeedCheckConcurrency {
		return nil, fmt.Errorf("dns.speed-check-concurrency must be between 1 and %d (or 0 for default)", maxSpeedCheckConcurrency)
	}
	if config.MaxReplyIPNum < 0 || int64(config.MaxReplyIPNum) > math.MaxInt32 {
		return nil, fmt.Errorf("dns.max-reply-ip-num must be between 1 and %d (or 0 for default)", int64(math.MaxInt32))
	}
	var modes []speedCheckMode
	if config.Mode == nil {
		config.Mode = []string{"auto"}
	}
	seen := make(map[speedCheckMode]bool)
	for _, value := range config.Mode {
		value = strings.TrimSpace(strings.ToLower(value))
		if value == "auto" {
			if len(config.Mode) != 1 {
				return nil, errors.New("dns.speed-check-mode: auto must be used alone")
			}
			modes = []speedCheckMode{{port: 443, tcpMode: tcpProbeAuto}, {port: 80, tcpMode: tcpProbeAuto}, {}}
			break
		}
		if value == "none" {
			return nil, errors.New("dns.speed-check-mode: none is no longer supported; use an empty value or [] to disable")
		}
		var mode speedCheckMode
		switch {
		case value == "ping":
		case value == "tcp" || value == "tcp-syn" || strings.HasPrefix(value, "tcp:") || strings.HasPrefix(value, "tcp-syn:"):
			name, portValue, hasPort := strings.Cut(value, ":")
			if !hasPort {
				portValue = "80"
			}
			port, err := strconv.ParseUint(portValue, 10, 16)
			if err != nil || port == 0 {
				return nil, fmt.Errorf("dns.speed-check-mode: invalid TCP port in %q", value)
			}
			if name == "tcp-syn" {
				if err := validateTCPSYNPlatform(); err != nil {
					return nil, err
				}
			}
			mode.port = uint16(port)
			if name == "tcp-syn" {
				mode.tcpMode = tcpProbeSYN
			}
		default:
			return nil, fmt.Errorf("dns.speed-check-mode: unsupported mode %q", value)
		}
		if !seen[mode] {
			seen[mode] = true
			modes = append(modes, mode)
		}
	}
	if len(modes) > maxSpeedCheckModes {
		return nil, fmt.Errorf("dns.speed-check-mode supports at most %d distinct stages", maxSpeedCheckModes)
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
	if config.MaxReplyIPNum == 0 {
		config.MaxReplyIPNum = DefaultMaxReplyIPNum
	}
	return &directSpeedChecker{
		modes: modes, timeout: config.Timeout, concurrency: config.Concurrency,
		responseMode:  normalizeResponseMode(config.ResponseMode),
		maxReplyIPNum: config.MaxReplyIPNum,
		slots:         make(chan struct{}, config.Concurrency), probe: probeDirectIP,
		pool:       newDNSProbePool(config.Concurrency, config.Timeout),
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

type dnsSpeedCheckDeadlineKey struct{}

// The two address families share one optimization budget without shortening
// either family's normal DNS context. A plain exchange starts its own budget.
func withSpeedCheckDeadline(ctx context.Context, timeout time.Duration) context.Context {
	if _, ok := ctx.Value(dnsSpeedCheckDeadlineKey{}).(time.Time); ok {
		return ctx
	}
	return context.WithValue(ctx, dnsSpeedCheckDeadlineKey{}, time.Now().Add(timeout))
}

func speedCheckDeadline(ctx context.Context, timeout time.Duration) time.Time {
	if deadline, ok := ctx.Value(dnsSpeedCheckDeadlineKey{}).(time.Time); ok {
		return deadline
	}
	return time.Now().Add(timeout)
}

// Exchange must only be called with an upstream pool selected for DIRECT. An
// existing non-DIRECT routing plan is also rejected defensively. The winning
// IP selects one CNAME chain. Only compatible unsigned terminal address RRsets
// may contribute other measured fast addresses; incompatible chains stay apart.
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

	// Upstream DNS and optional optimization have separate lifetimes. In
	// particular, reaching the optimization deadline without a reply must not
	// cancel the only outstanding DNS attempt or restart the upstream batch.
	ctx, cancel := context.WithTimeout(ctx, resolver.DefaultDNSTimeout)
	defer cancel()
	probeCtx, cancelProbes := context.WithDeadline(ctx, speedCheckDeadline(ctx, s.timeout))
	defer cancelProbes()
	probeDone := probeCtx.Done()
	optimizing := true
	mode := dnsResponseMode(ctx, s)
	earlyOffered := false
	type upstreamResult struct {
		message    *D.Msg
		err        error
		receivedAt time.Time
	}
	upstreams := make(chan upstreamResult, len(clients))
	for _, client := range clients {
		client := client
		go func() {
			if ctx.Err() != nil {
				return
			}
			message, err := client.ExchangeContext(ctx, query.Copy())
			result := upstreamResult{message: message, err: err, receivedAt: time.Now()}
			select {
			case upstreams <- result:
			case <-ctx.Done():
			}
		}()
	}
	stages := newSpeedCheckStages(probeCtx, s, probe)
	defer stages.close()
	stageTimer := time.NewTicker(speedCheckStageInterval)
	defer stageTimer.Stop()

	var firstResponse *D.Msg
	var firstAddressResponse *D.Msg
	var firstError error
	policyRejected := false
	var best *speedCheckResult
	var responses []*D.Msg
	measurements := make(map[netip.Addr]time.Duration)
	remaining := len(clients)
	finish := func() (*D.Msg, bool, error) {
		if best != nil {
			answer := speedCheckAnswer(ctx, *best, query.Question[0], responses, measurements, s.maxReplyIPNum)
			return answer, true, nil
		}
		if firstAddressResponse != nil {
			answer := speedCheckUnmeasuredAnswer(ctx, firstAddressResponse, query.Question[0], responses, s.maxReplyIPNum)
			return answer, true, nil
		}
		if firstResponse != nil {
			answer := speedCheckLimitAnswer(firstResponse, query.Question[0], s.maxReplyIPNum)
			inheritDNSAnswerLifetime(ctx, answer, firstResponse)
			return answer, true, nil
		}
		if firstError != nil {
			return nil, true, fmt.Errorf("all DNS requests failed: %w", firstError)
		}
		if err := ctx.Err(); err != nil {
			return nil, true, err
		}
		return nil, true, errors.New("all DNS requests failed")
	}
	recordProbe := func(attempt speedCheckAttemptResult) {
		result, waiting := stages.record(attempt)
		if !waiting {
			return
		}
		// A cached/shared result still belongs to this waiter's measurements.
		// Skipped and canceled attempts never become reachability evidence.
		if measurements, ok := ctx.Value(dnsProbeMeasurementsKey{}).(*dualStackMeasurements); ok {
			measurements.observe(result.candidate.ip, result.rtt, result.err)
		}
		if errors.Is(result.err, errTCPSYNUnavailable) {
			s.synWarning.Do(func() { log.Warnln("[DNS] %v", result.err) })
		}
		if result.err == nil && result.rtt >= 0 {
			if previous, exists := measurements[result.candidate.ip]; !exists || result.rtt < previous {
				measurements[result.candidate.ip] = result.rtt
			}
			if best == nil || result.rtt < best.rtt || (result.rtt == best.rtt && result.candidate.ip.Less(best.candidate.ip)) {
				best = &result
			}
		}
	}
	drainProbes := func() {
		for {
			select {
			case result := <-stages.results:
				recordProbe(result)
			default:
				return
			}
		}
	}
	for remaining > 0 || len(stages.attempts) > 0 || (optimizing && stages.canAdvance()) {
		if dnsQueryClosed(ctx) {
			return nil, true, tunnel.ErrDNSNativeQueryClosed
		}
		select {
		case <-ctx.Done():
			return finish()
		case <-probeDone:
			optimizing, probeDone = false, nil
			// Prefer any measurements already delivered when the timer fired.
			drainProbes()
			stages.close()
			if firstResponse != nil || policyRejected {
				return finish()
			}
		case <-stageTimer.C:
			if optimizing {
				// A delivered success ends stage escalation, even if its channel
				// became ready at exactly the same time as this tick.
				drainProbes()
				stages.advance()
			}
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
			recordDNSAnswerReceived(ctx, result.message, result.receivedAt)
			if !speedCheckAcceptResponse(ctx, result.message) {
				policyRejected = true
				if firstError == nil {
					firstError = errors.New("DNS upstream response requires fallback")
				}
				// A valid filtered response is enough to start normal fallback
				// once selection ends; only an unanswered DNS request needs the
				// remaining normal DNS budget.
				if !optimizing || probeCtx.Err() != nil {
					return finish()
				}
				continue
			}
			if firstResponse == nil {
				firstResponse = result.message
			}
			if !optimizing || probeCtx.Err() != nil {
				return finish()
			}
			if speedCheckProtected(result.message) {
				return result.message, true, nil
			}
			if !earlyOffered && mode == ResponseModeFastestResponse {
				earlyOffered = true
				answer := speedCheckLimitAnswer(result.message, query.Question[0], s.maxReplyIPNum)
				inheritDNSAnswerLifetime(ctx, answer, result.message)
				if !deliverDNSEarlyResponse(ctx, answer) {
					return answer, true, nil
				}
			}
			candidates := speedCheckCandidates(result.message, query.Question[0])
			if len(candidates) != 0 {
				responses = append(responses, result.message)
			}
			if firstAddressResponse == nil && len(candidates) != 0 {
				firstAddressResponse = result.message
			}
			for _, candidate := range candidates {
				stages.add(candidate)
			}
		case result := <-stages.results:
			if optimizing {
				recordProbe(result)
			}
		}
		if !earlyOffered && mode == ResponseModeFirstPing && best != nil {
			earlyOffered = true
			answer, _, _ := finish()
			if !deliverDNSEarlyResponse(ctx, answer) {
				return answer, true, nil
			}
		}
		if stages.skipped && len(stages.attempts) == 0 && !stages.canAdvance() && firstResponse != nil {
			return finish()
		}
	}
	return finish()
}

func speedCheckQuestion(message *D.Msg) bool {
	if message == nil || message.CheckingDisabled || speedCheckQueryProtected(message) {
		return false
	}
	if opt := message.IsEdns0(); opt != nil && opt.Do() {
		return false
	}
	return len(message.Question) == 1 && message.Opcode == D.OpcodeQuery &&
		message.Question[0].Qclass == D.ClassINET &&
		(message.Question[0].Qtype == D.TypeA || message.Question[0].Qtype == D.TypeAAAA || message.Question[0].Qtype == D.TypeHTTPS)
}

// HTTPS carries both families in one service record. Only address questions
// may create an auxiliary A/AAAA exchange or a family-preference cache entry.
func speedCheckAddressQuestion(message *D.Msg) bool {
	return speedCheckQuestion(message) && message.Question[0].Qtype != D.TypeHTTPS
}

func speedCheckResponseMatches(query, response *D.Msg) bool {
	if query == nil || len(query.Question) != 1 || response == nil || !response.Response ||
		response.Id != query.Id || response.Opcode != query.Opcode {
		return false
	}
	// Header-only errors are valid DNS responses, just as in the native wire
	// transport. Empty successes and errors carrying unrelated records are not.
	if len(response.Question) == 0 {
		return response.Rcode != D.RcodeSuccess && len(response.Answer)+len(response.Ns)+len(response.Extra) == 0
	}
	if len(response.Question) != 1 {
		return false
	}
	q, r := query.Question[0], response.Question[0]
	return strings.EqualFold(q.Name, r.Name) && q.Qtype == r.Qtype && q.Qclass == r.Qclass
}

func speedCheckCandidates(message *D.Msg, question D.Question) []speedCheckCandidate {
	chain, valid := speedCheckResponseChain(message, question)
	if !valid {
		return nil
	}
	owner := chain.owner
	if question.Qtype == D.TypeHTTPS {
		return speedCheckHTTPSCandidates(message, owner)
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

func speedCheckProtected(message *D.Msg) bool {
	return message != nil && (message.AuthenticatedData || speedCheckQueryProtected(message))
}

// AD in a query advertises that the client understands authenticated replies;
// it does not authenticate the question. Actual message signatures remain an
// end-to-end boundary for both questions and replies.
func speedCheckQueryProtected(message *D.Msg) bool {
	if message == nil {
		return false
	}
	if message.IsTsig() != nil {
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

func (s *directSpeedChecker) checkMode(ctx context.Context, ip netip.Addr, mode speedCheckMode, probe speedCheckProbe) (time.Duration, error) {
	if !dnsProbeContextActive(ctx) {
		return 0, errDNSProbeNotStarted
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return 0, errDNSProbeNotStarted
	default:
		return 0, errDNSProbeBusy
	}
	if !dnsProbeContextActive(ctx) {
		return 0, errDNSProbeNotStarted
	}
	return probe(ctx, ip, mode)
}

func probeDirectIP(ctx context.Context, ip netip.Addr, mode speedCheckMode) (time.Duration, error) {
	if mode.port == 0 {
		return pingDirectIP(ctx, ip)
	}
	// Numeric addresses avoid DNS recursion. The low-level dialer preserves
	// Mihomo's socket/interface policy without entering tunnel rule matching
	// or adding synthetic connection entries to the traffic panel.
	return probeTCPMode(ctx, mode.tcpMode, func(ctx context.Context) (time.Duration, error) {
		return probeTCPSYN(ctx, ip, mode.port)
	}, func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", netip.AddrPortFrom(ip, mode.port).String(), dialer.WithTFO(false))
	})
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

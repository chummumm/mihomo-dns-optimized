package dns

import (
	"context"
	"fmt"
	"sync"

	D "github.com/miekg/dns"
)

const (
	ResponseModeFirstPing              = "first-ping"
	ResponseModeFastestIP              = "fastest-ip"
	ResponseModeFastestResponse        = "fastest-response"
	dnsEarlyReplyTTL            uint32 = 3
)

// Run only for an answer published while the same exchange keeps optimizing,
// after each waiter's receipt-based aging. Cache snapshots retain their full
// lifetime, and later ordinary cache hits do not inherit this renewal interval.
func limitDNSEarlyReplyTTL(query, answer *D.Msg) *D.Msg {
	if !answerPolicyCanRewrite(query, answer) || answer.Rcode != D.RcodeSuccess ||
		query.Question[0].Qclass != D.ClassINET ||
		!speedCheckQuestion(query) ||
		len(speedCheckCandidates(answer, query.Question[0])) == 0 {
		return answer
	}
	for _, rr := range answer.Ns {
		if rr.Header().Rrtype == D.TypeSOA {
			return answer
		}
	}
	return (AnswerPolicy{RRTTLReplyMax: dnsEarlyReplyTTL}).applyReply(query, answer)
}

func ValidateResponseMode(mode string) error {
	switch mode {
	case "", ResponseModeFirstPing, ResponseModeFastestIP, ResponseModeFastestResponse:
		return nil
	default:
		return fmt.Errorf("dns.response-mode must be first-ping, fastest-ip or fastest-response")
	}
}

func normalizeResponseMode(mode string) string {
	if mode == "" {
		return ResponseModeFirstPing
	}
	return mode
}

type dnsEarlyResponseKey struct{}
type dnsResponseModeKey struct{}
type dnsEarlyReturnKey struct{}

// A callback returns true only when the resolver has taken ownership of the
// remaining work. Otherwise the caller returns this valid answer immediately
// and cancels its optional work through the ordinary exchange cleanup.
type dnsEarlyResponse func(*D.Msg) bool

func withDNSEarlyResponse(ctx context.Context, deliver dnsEarlyResponse) context.Context {
	return context.WithValue(ctx, dnsEarlyResponseKey{}, deliver)
}

func dnsResponseMode(ctx context.Context, checker *directSpeedChecker) string {
	if value, _ := ctx.Value(dnsResponseModeKey{}).(string); value != "" {
		return value
	}
	if cacheBackground(ctx) {
		return ResponseModeFastestIP
	}
	return checker.responseMode
}

func deliverDNSEarlyResponse(ctx context.Context, message *D.Msg) bool {
	deliver, _ := ctx.Value(dnsEarlyResponseKey{}).(dnsEarlyResponse)
	if deliver != nil && deliver(message) {
		return true
	}
	if returned, _ := ctx.Value(dnsEarlyReturnKey{}).(*bool); returned != nil {
		*returned = true
	}
	return false
}

// Parallel fallback may have an answer before main has been rejected. Hold one
// immutable offer until ipExchange authorizes that branch; never publish a
// fallback answer merely because it arrived before the main pool.
type dnsEarlyResponseGate struct {
	mu      sync.Mutex
	active  bool
	message *D.Msg
	deliver dnsEarlyResponse
	stop    context.CancelFunc
}

func newDNSEarlyResponseGate(ctx context.Context) (context.Context, *dnsEarlyResponseGate) {
	child, cancel := context.WithCancel(ctx)
	deliver, _ := ctx.Value(dnsEarlyResponseKey{}).(dnsEarlyResponse)
	gate := &dnsEarlyResponseGate{deliver: deliver, stop: cancel}
	return withDNSEarlyResponse(child, gate.offer), gate
}

func (g *dnsEarlyResponseGate) offer(message *D.Msg) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.active {
		g.message = message
		return true
	}
	return g.deliver != nil && g.deliver(message)
}

func (g *dnsEarlyResponseGate) activate() *D.Msg {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active = true
	if g.message != nil && (g.deliver == nil || !g.deliver(g.message)) {
		g.stop()
		return g.message
	}
	return nil
}

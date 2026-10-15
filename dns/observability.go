package dns

import (
	"context"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/component/dnsstats"
	"github.com/metacubex/mihomo/component/resolver"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/tunnel"

	D "github.com/miekg/dns"
)

// Shared work must not overwrite its first client's result classification.
func markDNSOutcome(ctx context.Context, outcome dnsstats.Outcome) {
	if icontext.DNSBootstrap(ctx) || cacheBackground(ctx) {
		return
	}
	dnsstats.MarkOutcome(ctx, outcome)
}

func dnsQuestionType(qtype uint16) string {
	if value, ok := D.TypeToString[qtype]; ok {
		return value
	}
	return "TYPE" + strconv.Itoa(int(qtype))
}

func dnsResponseCode(rcode int) string {
	if value, ok := D.RcodeToString[rcode]; ok {
		return value
	}
	return strconv.Itoa(rcode)
}

func dnsObservationRecord(ctx context.Context, question D.Question) dnsstats.Record {
	source := dnsstats.SourceFromContext(ctx)
	if metadata := icontext.DNSRoutingMetadata(ctx); metadata != nil {
		if metadata.SrcIP.IsValid() {
			source.Client = metadata.SrcIP.Unmap().String()
		}
		source.Protocol = metadata.NetWork.String()
		if metadata.InName != "" {
			source.Name = metadata.InName
		}
	}
	return dnsstats.Record{QName: question.Name, QType: dnsQuestionType(question.Qtype), Client: source.Client, Protocol: source.Protocol, Source: source.Name}
}

func finishDNSObservation(observation *dnsstats.Query, record dnsstats.Record, ctx *icontext.DNSContext, message *D.Msg, err error) {
	if observation == nil {
		return
	}
	resolution := observation.Outcome()
	record.Upstreams = observation.Upstreams()
	if len(record.Upstreams) == 1 {
		record.Upstream = record.Upstreams[0].Address
	}
	record.Outcome = resolution.String()
	switch resolution {
	case dnsstats.CacheFresh:
		record.Cache = "fresh"
	case dnsstats.CacheStale:
		record.Cache = "stale"
	case dnsstats.Upstream:
		record.Cache = "miss"
	default:
		record.Cache = "none"
	}
	switch ctx.Type() {
	case icontext.DNSTypeHost:
		record.Outcome = dnsstats.Hosts.String()
	case icontext.DNSTypeFakeIP:
		record.Outcome = dnsstats.FakeIP.String()
	}
	if err != nil {
		switch {
		case errors.Is(err, resolver.ErrDNSDrop):
			record.Outcome = dnsstats.Drop.String()
		case errors.Is(err, context.DeadlineExceeded):
			record.Outcome = dnsstats.Error.String()
			record.Error = "timeout"
		case errors.Is(err, context.Canceled), errors.Is(err, tunnel.ErrDNSNativeQueryClosed):
			record.Outcome = dnsstats.Error.String()
			record.Error = "canceled"
		default:
			record.Outcome = dnsstats.Error.String()
			record.Error = "resolution_failed"
		}
		if !errors.Is(err, resolver.ErrDNSDrop) {
			record.RCode = dnsResponseCode(D.RcodeServerFailure)
		}
	} else if message != nil {
		record.RCode = dnsResponseCode(message.Rcode)
		record.Answers, record.AnswersTruncated = summarizeDNSAnswers(message.Answer)
		if message.Rcode != D.RcodeSuccess && message.Rcode != D.RcodeNameError && !(message.Rcode == D.RcodeRefused && resolution == dnsstats.Reject) {
			record.Outcome = dnsstats.Error.String()
			record.Error = "rcode_" + strings.ToLower(record.RCode)
		}
	} else {
		record.Outcome = dnsstats.Error.String()
		record.Error = "empty_response"
	}
	observation.Complete(record)
}

// Copy bounded summaries; never retain messages, large TXT/DNSSEC/EDNS values.
func summarizeDNSAnswers(records []D.RR) ([]dnsstats.Answer, bool) {
	truncated := len(records) > dnsstats.MaxAnswers
	if truncated {
		records = records[:dnsstats.MaxAnswers]
	}
	answers := make([]dnsstats.Answer, 0, len(records))
	for _, rr := range records {
		if rr == nil {
			continue
		}
		var value string
		switch rr := rr.(type) {
		case *D.A:
			value = rr.A.String()
		case *D.AAAA:
			value = rr.AAAA.String()
		case *D.CNAME:
			value = rr.Target
		case *D.NS:
			value = rr.Ns
		case *D.PTR:
			value = rr.Ptr
		case *D.MX:
			value = strconv.Itoa(int(rr.Preference)) + " " + rr.Mx
		case *D.SRV:
			value = strconv.Itoa(int(rr.Priority)) + " " + strconv.Itoa(int(rr.Weight)) + " " + strconv.Itoa(int(rr.Port)) + " " + rr.Target
		case *D.SOA:
			value = rr.Ns + " " + rr.Mbox + " " + strconv.FormatUint(uint64(rr.Serial), 10)
		case *D.TXT:
			var text strings.Builder
			for i, part := range rr.Txt {
				if i > 0 {
					text.WriteByte(' ')
				}
				remaining := dnsstats.MaxAnswerBytes + 1 - text.Len()
				if remaining <= 0 {
					truncated = true
					break
				}
				if len(part) > remaining {
					part = part[:remaining]
					truncated = true
				}
				text.WriteString(part)
			}
			value = text.String()
		case *D.HTTPS:
			value = rr.Target
		case *D.SVCB:
			value = rr.Target
		default:
			value = "[record data omitted]"
			truncated = true
		}
		if len(value) > dnsstats.MaxAnswerBytes {
			value = value[:dnsstats.MaxAnswerBytes]
			truncated = true
		}
		answers = append(answers, dnsstats.Answer{Type: dnsQuestionType(rr.Header().Rrtype), Value: value, TTL: rr.Header().Ttl})
	}
	return answers, truncated
}

// Measure concrete protocol-client exchanges below all DNS answer/candidate
// caches. Protocol recovery retries remain one resolver exchange.
type dnsUpstreamAttempt struct {
	attempt *dnsstats.Attempt
	ctx     context.Context
	source  dnsstats.UpstreamSource
}

func observeDNSUpstream(ctx context.Context, address string, dialers ...*dnsDialer) *dnsUpstreamAttempt {
	// Infrastructure resolution is excluded by purpose, not endpoint: the same
	// server remains visible when it answers an ordinary business question.
	if icontext.DNSBootstrap(ctx) {
		return nil
	}
	var outbound dnsstats.Outbound
	if len(dialers) != 0 {
		outbound = dialers[0].ObservabilityOutbound()
	} else if route := queryRoute(ctx); route != nil && route.err == nil {
		outbound = route.plan.ObservabilityOutbound()
	}
	attempt := dnsstats.StartUpstream(ctx, address, outbound)
	if attempt == nil {
		return nil
	}
	return &dnsUpstreamAttempt{attempt: attempt, ctx: ctx, source: dnsstats.NewUpstreamSource(address, outbound)}
}

func finishDNSUpstream(observed *dnsUpstreamAttempt, message *D.Msg, err error) {
	if observed == nil {
		return
	}
	attempt := observed.attempt
	if attempt == nil {
		return
	}
	var timeout net.Error
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, tunnel.ErrDNSNativeQueryClosed):
		attempt.Complete(dnsstats.UpstreamCanceled)
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()):
		attempt.Complete(dnsstats.UpstreamTimeout)
	case err != nil || message == nil:
		attempt.Complete(dnsstats.UpstreamError)
	case message.Rcode != D.RcodeSuccess && message.Rcode != D.RcodeNameError:
		attempt.Complete(dnsstats.UpstreamRCodeError)
	default:
		attempt.Complete(dnsstats.UpstreamSuccess)
	}
	if err == nil && message != nil {
		setDNSAnswerSources(observed.ctx, message, []dnsstats.UpstreamSource{observed.source})
	}
}

// Provenance belongs to one shared exchange. Copies and merged answers inherit
// only contributing responses; the immutable result is delivered to each
// waiter's own query event, never to the first waiter's detached event.
type dnsAnswerSourcesKey struct{}
type dnsAnswerSources struct {
	mu      sync.Mutex
	answers map[*D.Msg][]dnsstats.UpstreamSource
}

func withDNSAnswerSources(ctx context.Context) context.Context {
	return context.WithValue(ctx, dnsAnswerSourcesKey{}, &dnsAnswerSources{})
}

func dnsAnswerSourcesFor(ctx context.Context, message *D.Msg) []dnsstats.UpstreamSource {
	s, _ := ctx.Value(dnsAnswerSourcesKey{}).(*dnsAnswerSources)
	if s == nil || message == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]dnsstats.UpstreamSource(nil), s.answers[message]...)
}

func setDNSAnswerSources(ctx context.Context, message *D.Msg, sources []dnsstats.UpstreamSource) {
	s, _ := ctx.Value(dnsAnswerSourcesKey{}).(*dnsAnswerSources)
	if s == nil || message == nil || len(sources) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answers == nil {
		s.answers = make(map[*D.Msg][]dnsstats.UpstreamSource)
	}
	merged := s.answers[message]
	for _, source := range sources {
		// One extra entry carries truncation to the bounded query record.
		if len(merged) > dnsstats.MaxQueryUpstreams {
			break
		}
		found := false
		for _, previous := range merged {
			if previous == source {
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, source)
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		a, b := merged[i], merged[j]
		if a.Address != b.Address {
			return a.Address < b.Address
		}
		if a.Outbound != b.Outbound {
			return a.Outbound < b.Outbound
		}
		if a.OutboundType != b.OutboundType {
			return a.OutboundType < b.OutboundType
		}
		return !a.Reused && b.Reused
	})
	s.answers[message] = merged
}

func inheritDNSAnswerSources(ctx context.Context, message, original *D.Msg) {
	if message != original {
		setDNSAnswerSources(ctx, message, dnsAnswerSourcesFor(ctx, original))
	}
}

// A shared result carries immutable provenance to every waiter. The work-local
// set grows only with replies from this exchange's configured client fan-out
// and its answer-policy copies; it never retains query history. An arbitrary
// slot cap would misclassify a late local winner or its copy as an upstream
// answer. No DNS message field, matcher, or first-client event is changed.
type dnsExchangeResult struct {
	*D.Msg
	local      bool
	earlyReply bool
	receivedAt time.Time
	expires    time.Time
	upstreams  []dnsstats.UpstreamSource
}

type dnsLocalAnswersKey struct{}
type dnsLocalAnswers struct {
	mu     sync.Mutex
	values map[*D.Msg]struct{}
}

func (a *dnsLocalAnswers) has(message *D.Msg) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.values[message]
	return ok
}

func markDNSLocalAnswer(ctx context.Context, message *D.Msg) {
	a, _ := ctx.Value(dnsLocalAnswersKey{}).(*dnsLocalAnswers)
	if a == nil || message == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.values == nil {
		a.values = make(map[*D.Msg]struct{})
	}
	a.values[message] = struct{}{}
}

func inheritDNSLocalAnswer(ctx context.Context, message, original *D.Msg) {
	a, _ := ctx.Value(dnsLocalAnswersKey{}).(*dnsLocalAnswers)
	if a != nil && a.has(original) {
		markDNSLocalAnswer(ctx, message)
	}
}

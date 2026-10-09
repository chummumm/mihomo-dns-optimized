package dns

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	C "github.com/metacubex/mihomo/constant"

	D "github.com/miekg/dns"
)

// DualStackConfig optionally compares address families inside an already
// selected DIRECT pool. Zero values disable it. Threshold is an inclusive RTT
// difference; callers supply the 10ms configuration default explicitly so that
// an intentional zero threshold remains representable.
type DualStackConfig struct {
	Enabled        bool
	Threshold      time.Duration
	AllowForceAAAA bool
}

func ValidateDualStackConfig(config DualStackConfig) error {
	if config.Threshold < 0 || config.Threshold > time.Second {
		return errors.New("dns.dualstack-ip-selection-threshold must be between 0 and 1000 milliseconds")
	}
	return nil
}

type dualStackMeasurements struct {
	mu   sync.Mutex
	rtts map[netip.Addr]time.Duration
}

func (m *dualStackMeasurements) wrap(probe speedCheckProbe) speedCheckProbe {
	return func(ctx context.Context, ip netip.Addr, mode speedCheckMode) (time.Duration, error) {
		rtt, err := probe(ctx, ip, mode)
		if err == nil && rtt >= 0 {
			m.mu.Lock()
			previous, exists := m.rtts[ip]
			if !exists || rtt < previous {
				m.rtts[ip] = rtt
			}
			m.mu.Unlock()
		}
		return rtt, err
	}
}

func (m *dualStackMeasurements) fastest(query, response *D.Msg) (time.Duration, bool) {
	if !speedCheckResponseMatches(query, response) || response.CheckingDisabled || speedCheckProtected(response) {
		return 0, false
	}
	if opt := response.IsEdns0(); opt != nil && opt.Do() {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var best time.Duration
	found := false
	for _, candidate := range speedCheckCandidates(response, query.Question[0]) {
		if rtt, ok := m.rtts[candidate.ip]; ok && (!found || rtt < best) {
			best, found = rtt, true
		}
	}
	return best, found
}

// ExchangeDualStackWithProbe shares the original query's upstream pool, route
// and total deadline with one auxiliary question. It never calls the resolver
// recursively, selects a second outbound, or writes an auxiliary cache entry.
// Existing checker slots bound all probes across both families and callers.
func (s *directSpeedChecker) ExchangeDualStackWithProbe(ctx context.Context, clients []dnsClient, query *D.Msg, probe speedCheckProbe, config DualStackConfig) (*D.Msg, bool, error) {
	eligible := s != nil && probe != nil && config.Enabled && query != nil && speedCheckQuestion(query)
	if eligible && query.Question[0].Qtype == D.TypeA && !config.AllowForceAAAA {
		eligible = false // Preserve IPv4 for applications without IPv6 support.
	}
	if route := queryRoute(ctx); route != nil && (route.plan == nil || (route.plan.Type() != C.Direct && route.plan.Type() != C.Compatible)) {
		eligible = false
	}
	if !eligible {
		return s.ExchangeWithProbe(ctx, clients, query, probe)
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	other := query.Copy()
	if query.Question[0].Qtype == D.TypeAAAA {
		other.Question[0].Qtype = D.TypeA
	} else {
		other.Question[0].Qtype = D.TypeAAAA
	}
	type result struct {
		message *D.Msg
		cache   bool
		err     error
	}
	primaryMeasurements := &dualStackMeasurements{rtts: make(map[netip.Addr]time.Duration)}
	otherMeasurements := &dualStackMeasurements{rtts: make(map[netip.Addr]time.Duration)}
	otherResult := make(chan result, 1)
	go func() {
		message, cache, err := s.ExchangeWithProbe(ctx, clients, other, otherMeasurements.wrap(probe))
		otherResult <- result{message, cache, err}
	}()
	message, cache, err := s.ExchangeWithProbe(ctx, clients, query, primaryMeasurements.wrap(probe))
	if err != nil {
		return message, cache, err
	}
	primaryRTT, primaryMeasured := primaryMeasurements.fastest(query, message)
	if !primaryMeasured {
		return message, cache, nil // Unknown speed and signed answers are never filtered.
	}
	var auxiliary result
	select {
	case auxiliary = <-otherResult:
	case <-ctx.Done():
		return message, cache, nil
	}
	if auxiliary.err != nil {
		return message, cache, nil
	}
	otherRTT, otherMeasured := otherMeasurements.fastest(other, auxiliary.message)
	if !otherMeasured || primaryRTT-otherRTT < config.Threshold {
		return message, cache, nil
	}
	return dualStackNoData(query, message), false, nil
}

func dualStackNoData(query, original *D.Msg) *D.Msg {
	message := new(D.Msg).SetReply(query)
	message.RecursionAvailable = original.RecursionAvailable
	// A zero-TTL SOA gives a transient NODATA answer without claiming that the
	// domain does not exist. In particular, this measured preference must not
	// become a negative entry eligible for seven days of serve-expired reuse.
	message.Ns = []D.RR{&D.SOA{
		Hdr: D.RR_Header{Name: query.Question[0].Name, Rrtype: D.TypeSOA, Class: D.ClassINET, Ttl: 0},
		Ns:  ".", Mbox: ".", Minttl: 0,
	}}
	if opt := original.IsEdns0(); opt != nil {
		message.Extra = []D.RR{D.Copy(opt)}
	}
	return message
}

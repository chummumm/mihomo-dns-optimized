package dns

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	D "github.com/miekg/dns"
)

// AnswerPolicy contains optional native-answer adjustments. Infrastructure
// resolvers and ordinary proxy traffic do not inherit these business settings.
// Zero values retain the original answer and its authoritative lifetimes.
type AnswerPolicy struct {
	ForceNoCNAME bool
	RRTTLMin     uint32
	RRTTLMax     uint32
}

func (p AnswerPolicy) enabled() bool {
	return p.ForceNoCNAME || p.RRTTLMin != 0 || p.RRTTLMax != 0
}

// Automatic QNAME routing already keys the complete wire question. Optional
// answer rewriting can also be used while routing is bypassed (for example
// Global mode or an explicit DNS transport). Those answers must not mix DO/CD,
// EDNS or signed queries through the upstream question-only cache/flight key.
func (r *Resolver) cacheKey(ctx context.Context, query *D.Msg) string {
	key := dnsCacheKey(ctx, query.Question[0])
	if queryRoute(ctx) != nil || !r.answerPolicy.enabled() {
		return key
	}
	copy := query.Copy()
	copy.Id = 0
	wire, err := copy.Pack()
	if err != nil {
		// An invalid message cannot produce a cacheable successful exchange.
		return key + "|answer-policy:invalid"
	}
	return fmt.Sprintf("%s|answer-policy:%x", key, sha256.Sum256(wire))
}

func (p AnswerPolicy) apply(query, answer *D.Msg) *D.Msg {
	if !p.enabled() ||
		query == nil || answer == nil || len(query.Question) != 1 ||
		query.Opcode != D.OpcodeQuery || !speedCheckResponseMatches(query, answer) ||
		query.CheckingDisabled || answer.Truncated || speedCheckProtected(query) || speedCheckProtected(answer) {
		return answer
	}
	if opt := query.IsEdns0(); opt != nil && opt.Do() {
		return answer
	}
	if answer.Rcode != D.RcodeSuccess && answer.Rcode != D.RcodeNameError {
		return answer
	}
	result := answer.Copy()
	for _, records := range [][]D.RR{result.Answer, result.Ns, result.Extra} {
		for _, rr := range records {
			if rr.Header().Rrtype == D.TypeOPT {
				continue // OPT's TTL field contains EDNS flags, not a lifetime.
			}
			rr.Header().Ttl = p.clampTTL(rr.Header().Ttl)
			if soa, ok := rr.(*D.SOA); ok && p.RRTTLMax != 0 && soa.Minttl > p.RRTTLMax {
				soa.Minttl = p.RRTTLMax
			}
		}
	}
	if p.ForceNoCNAME && result.Rcode == D.RcodeSuccess {
		flattenAddressCNAME(result, query.Question[0])
	}
	return result
}

func (p AnswerPolicy) clampTTL(ttl uint32) uint32 {
	if ttl == 0 {
		return 0 // Do not make a non-cacheable answer cacheable.
	}
	if p.RRTTLMin != 0 && ttl < p.RRTTLMin {
		ttl = p.RRTTLMin
	}
	if p.RRTTLMax != 0 && ttl > p.RRTTLMax {
		ttl = p.RRTTLMax
	}
	return ttl
}

// Flatten only a complete, unambiguous IN A/AAAA chain in one answer. The
// resolver must never invent an address, follow a new rule, or join answers
// from different upstreams merely to hide a CNAME. CNAME questions, DNAME,
// partial/cyclic chains and unrelated records retain their original form.
func flattenAddressCNAME(message *D.Msg, question D.Question) {
	if question.Qclass != D.ClassINET || (question.Qtype != D.TypeA && question.Qtype != D.TypeAAAA) {
		return
	}
	cnames := make(map[string]string)
	for _, rr := range message.Answer {
		if rr.Header().Class != D.ClassINET {
			return
		}
		if cname, ok := rr.(*D.CNAME); ok {
			owner, target := strings.ToLower(cname.Hdr.Name), strings.ToLower(cname.Target)
			if old, exists := cnames[owner]; exists && old != target {
				return
			}
			cnames[owner] = target
		} else if rr.Header().Rrtype != question.Qtype {
			return
		}
	}
	if len(cnames) == 0 || len(cnames) > 16 {
		return
	}
	owner := strings.ToLower(question.Name)
	visited := make(map[string]bool)
	for cnames[owner] != "" {
		if visited[owner] {
			return
		}
		visited[owner] = true
		owner = cnames[owner]
	}
	if len(visited) != len(cnames) {
		return
	}
	var addresses []D.RR
	lifetime := minimalTTL(message.Answer)
	for _, rr := range message.Answer {
		if rr.Header().Rrtype == D.TypeCNAME {
			continue
		}
		if !strings.EqualFold(rr.Header().Name, owner) {
			return
		}
		if _, valid := speedCheckAddress(rr); !valid {
			return
		}
		addresses = append(addresses, rr)
	}
	if len(addresses) == 0 {
		return
	}
	for _, rr := range addresses {
		rr.Header().Name = question.Name
		rr.Header().Ttl = lifetime
	}
	message.Answer = addresses
	message.Authoritative = false
}

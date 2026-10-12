package dns

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/metacubex/mihomo/component/dnsstats"
	icontext "github.com/metacubex/mihomo/context"

	D "github.com/miekg/dns"
)

// ForcedQtypes is a bounded bitmap of all 16-bit DNS query types. Configuration
// builds it once; resolvers share the immutable value without a per-query map.
type ForcedQtypes [1024]uint64

func (types *ForcedQtypes) Contains(qtype uint16) bool {
	return types != nil && types[qtype/64]&(uint64(1)<<(qtype%64)) != 0
}

// AnswerPolicy contains optional native-answer adjustments. Infrastructure
// resolvers and ordinary proxy traffic do not inherit these business settings.
// Zero values retain the original answer and its authoritative lifetimes.
type AnswerPolicy struct {
	ForceNoCNAME  bool
	ForceAAAASOA  bool
	ForceQtypeSOA *ForcedQtypes
	RRTTL         uint32
	RRTTLMin      uint32
	RRTTLMax      uint32
	RRTTLReplyMax uint32
}

func (p AnswerPolicy) enabled() bool {
	return p.ForceNoCNAME || p.RRTTL != 0 || p.RRTTLMin != 0 || p.RRTTLMax != 0 || p.RRTTLReplyMax != 0
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
	if (!p.ForceNoCNAME && p.RRTTL == 0 && p.RRTTLMin == 0 && p.RRTTLMax == 0) || !answerPolicyCanRewrite(query, answer) {
		return answer
	}
	result := answer.Copy()
	for _, records := range [][]D.RR{result.Answer, result.Ns, result.Extra} {
		for _, rr := range records {
			if rr.Header().Rrtype == D.TypeOPT {
				continue // OPT's TTL field contains EDNS flags, not a lifetime.
			}
			rr.Header().Ttl = p.clampTTL(rr.Header().Ttl)
			if soa, ok := rr.(*D.SOA); ok {
				if p.RRTTL != 0 && soa.Minttl != 0 {
					soa.Minttl = p.RRTTL
				} else if p.RRTTLMax != 0 && soa.Minttl > p.RRTTLMax {
					soa.Minttl = p.RRTTLMax
				}
			}
		}
	}
	if p.ForceNoCNAME && result.Rcode == D.RcodeSuccess {
		flattenAddressCNAME(result, query.Question[0])
	}
	return result
}

func answerPolicyCanRewrite(query, answer *D.Msg) bool {
	if query == nil || answer == nil || len(query.Question) != 1 ||
		query.Opcode != D.OpcodeQuery || !speedCheckResponseMatches(query, answer) ||
		query.CheckingDisabled || answer.Truncated || speedCheckProtected(query) || speedCheckProtected(answer) {
		return false
	}
	if opt := query.IsEdns0(); opt != nil && opt.Do() {
		return false
	}
	return answer.Rcode == D.RcodeSuccess || answer.Rcode == D.RcodeNameError
}

// Reply-only TTL limits are applied after cache storage and after cache aging.
// Lowering a client's renewal interval must not lower our cache lifetime or
// refill a stale/zero-TTL answer. Always copy: singleflight/cache snapshots can
// still be serving another caller or a background selection.
func (p AnswerPolicy) applyReply(query, answer *D.Msg) *D.Msg {
	if p.RRTTLReplyMax == 0 || !answerPolicyCanRewrite(query, answer) {
		return answer
	}
	result := answer.Copy()
	for _, records := range [][]D.RR{result.Answer, result.Ns, result.Extra} {
		for _, rr := range records {
			if rr.Header().Rrtype != D.TypeOPT && rr.Header().Ttl > p.RRTTLReplyMax {
				rr.Header().Ttl = p.RRTTLReplyMax
			}
			if soa, ok := rr.(*D.SOA); ok && soa.Minttl > p.RRTTLReplyMax {
				soa.Minttl = p.RRTTLReplyMax
			}
		}
	}
	return result
}

// Hosts aliases can prepend CNAME records after the resolver has finished.
// Reconcile the complete client reply without reapplying cache TTL policy.
func (p AnswerPolicy) applyFinalReply(query, answer *D.Msg) *D.Msg {
	result := p.applyReply(query, answer)
	if !p.ForceNoCNAME || !answerPolicyCanRewrite(query, result) {
		return result
	}
	if result == answer {
		result = answer.Copy()
	}
	flattenAddressCNAME(result, query.Question[0])
	return result
}

func (p AnswerPolicy) clampTTL(ttl uint32) uint32 {
	if ttl == 0 {
		return 0 // Do not make a non-cacheable answer cacheable.
	}
	if p.RRTTL != 0 {
		return p.RRTTL // SmartDNS's fixed TTL takes precedence over min/max.
	}
	if p.RRTTLMin != 0 && ttl < p.RRTTLMin {
		ttl = p.RRTTLMin
	}
	if p.RRTTLMax != 0 && ttl > p.RRTTLMax {
		ttl = p.RRTTLMax
	}
	return ttl
}

func (p AnswerPolicy) forcesQtype(qtype uint16) bool {
	return (qtype == D.TypeAAAA && p.ForceAAAASOA) || p.ForceQtypeSOA.Contains(qtype)
}

func forcedSOAQuestion(ctx context.Context, query *D.Msg) bool {
	if icontext.DNSBootstrap(ctx) || query == nil || len(query.Question) != 1 ||
		query.Response || query.Opcode != D.OpcodeQuery || query.Question[0].Qclass != D.ClassINET || query.IsTsig() != nil {
		return false
	}
	// AD in a query advertises the client's DNSSEC capability; it does not
	// authenticate this transaction and must not bypass an explicit local block.
	for _, records := range [][]D.RR{query.Answer, query.Ns, query.Extra} {
		for _, rr := range records {
			if rr.Header().Rrtype == D.TypeRRSIG || rr.Header().Rrtype == D.TypeSIG {
				return false
			}
		}
	}
	return true
}

// An explicit local query-type policy returns NOERROR/NODATA, not NXDOMAIN or
// a fabricated upstream error. The 30-second local SOA matches SmartDNS; it is
// generated afresh and never stored as a serve-expired cache entry.
func (p AnswerPolicy) forcedSOAReply(ctx context.Context, query *D.Msg) *D.Msg {
	ttl := uint32(30)
	if p.RRTTLReplyMax != 0 && p.RRTTLReplyMax < ttl {
		ttl = p.RRTTLReplyMax
	}
	answer := new(D.Msg).SetReply(query)
	answer.RecursionAvailable = true
	answer.CheckingDisabled = query.CheckingDisabled
	answer.AuthenticatedData = false
	answer.Ns = []D.RR{&D.SOA{
		Hdr: D.RR_Header{Name: query.Question[0].Name, Rrtype: D.TypeSOA, Class: D.ClassINET, Ttl: ttl},
		Ns:  ".", Mbox: ".", Minttl: ttl,
	}}
	if opt := query.IsEdns0(); opt != nil {
		answer.SetEdns0(1232, opt.Do())
	}
	markDNSOutcome(ctx, dnsstats.Local)
	return answer
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

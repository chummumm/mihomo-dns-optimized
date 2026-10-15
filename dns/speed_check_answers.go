package dns

import (
	"context"
	"net/netip"
	"sort"
	"strings"
	"time"

	D "github.com/miekg/dns"
)

type speedCheckAlias struct {
	owner, target string
}

type speedCheckChain struct {
	owner string
	links []speedCheckAlias
}

// The complete path from the question to its terminal owner is the answer's
// identity. A common terminal name alone cannot make two CNAME paths compatible.
func speedCheckResponseChain(message *D.Msg, question D.Question) (speedCheckChain, bool) {
	if message == nil || message.Rcode != D.RcodeSuccess || message.Truncated {
		return speedCheckChain{}, false
	}
	cnames := make(map[string]string)
	for _, rr := range message.Answer {
		if cname, ok := rr.(*D.CNAME); ok && cname.Hdr.Class == D.ClassINET {
			owner, target := strings.ToLower(cname.Hdr.Name), strings.ToLower(cname.Target)
			if old, exists := cnames[owner]; exists && old != target {
				return speedCheckChain{}, false
			}
			cnames[owner] = target
		}
	}
	chain := speedCheckChain{owner: strings.ToLower(question.Name)}
	visited := make(map[string]bool)
	for cnames[chain.owner] != "" {
		if visited[chain.owner] {
			return speedCheckChain{}, false
		}
		visited[chain.owner] = true
		target := cnames[chain.owner]
		chain.links = append(chain.links, speedCheckAlias{chain.owner, target})
		chain.owner = target
	}
	return chain, true
}

func (c speedCheckChain) matches(other speedCheckChain) bool {
	if c.owner != other.owner || len(c.links) != len(other.links) {
		return false
	}
	for i, link := range c.links {
		if link != other.links[i] {
			return false
		}
	}
	return true
}

// SmartDNS stores RTTs in tenths of a millisecond. Its multi-address result
// retains measured addresses close to the fastest, including the small-RTT
// tolerances, instead of appending every reachable but substantially slower IP.
func speedCheckNearFastest(best, candidate time.Duration) bool {
	bestTicks, candidateTicks := best/(100*time.Microsecond), candidate/(100*time.Microsecond)
	return candidateTicks-bestTicks < 50 || candidateTicks < 100 || candidateTicks <= bestTicks+bestTicks/10+5
}

type speedCheckAnswerRecord struct {
	ip      netip.Addr
	rtt     time.Duration
	record  D.RR
	sources map[*D.Msg]bool
}

func speedCheckAnswer(ctx context.Context, best speedCheckResult, question D.Question, responses []*D.Msg, measurements map[netip.Addr]time.Duration, limit int) *D.Msg {
	winning := best.candidate.response
	if question.Qtype == D.TypeHTTPS {
		return speedCheckHTTPSAnswer(ctx, winning, question, measurements)
	}
	message := winning.Copy()
	chain, valid := speedCheckResponseChain(winning, question)
	if !valid || speedCheckProtected(winning) {
		inheritDNSAnswerLifetime(ctx, message, winning)
		return message
	}
	if limit == 0 {
		limit = DefaultMaxReplyIPNum
	}
	addresses := make(map[netip.Addr]*speedCheckAnswerRecord)
	for _, response := range responses {
		other, valid := speedCheckResponseChain(response, question)
		if !valid || speedCheckProtected(response) || !chain.matches(other) {
			continue
		}
		for _, rr := range response.Answer {
			if !speedCheckTerminalAddress(rr, chain.owner, question.Qtype) {
				continue
			}
			ip, valid := speedCheckAddress(rr)
			rtt, measured := measurements[ip]
			if !valid || !measured || !speedCheckNearFastest(best.rtt, rtt) {
				continue
			}
			if address := addresses[ip]; address != nil {
				// Equal addresses received with different TTLs are one candidate.
				// Keep the shortest lifetime, never a later upstream's extension.
				if rr.Header().Ttl < address.record.Header().Ttl {
					address.record.Header().Ttl = rr.Header().Ttl
				}
				address.sources[response] = true
			} else {
				addresses[ip] = &speedCheckAnswerRecord{ip: ip, rtt: rtt, record: D.Copy(rr), sources: map[*D.Msg]bool{response: true}}
			}
		}
	}
	ordered := make([]*speedCheckAnswerRecord, 0, len(addresses))
	for _, address := range addresses {
		ordered = append(ordered, address)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].rtt != ordered[j].rtt {
			return ordered[i].rtt < ordered[j].rtt
		}
		return ordered[i].ip.Less(ordered[j].ip)
	})
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	if len(ordered) == 0 {
		// The winning probe must have a source answer. Preserve that valid
		// response defensively if a caller supplies incomplete measurements.
		inheritDNSAnswerLifetime(ctx, message, winning)
		return message
	}
	contributing := map[*D.Msg]bool{winning: true}
	records := make([]D.RR, 0, len(ordered))
	for _, address := range ordered {
		records = append(records, address.record)
		for source := range address.sources {
			contributing[source] = true
		}
	}
	message.Answer = speedCheckReplaceAddresses(message.Answer, chain.owner, question.Qtype, records)

	// Keep the chosen chain and all other sections from one intact response.
	// Matching source chains can only shorten its TTLs. All contributing raw
	// records retain the earliest original receipt, so later TTL policy is
	// applied once before aging, without reviving old candidate lifetimes.
	aliasTTLs := make(map[speedCheckAlias]uint32)
	for source := range contributing {
		inheritDNSAnswerLifetime(ctx, message, source)
		for _, rr := range source.Answer {
			if cname, ok := rr.(*D.CNAME); ok && cname.Hdr.Class == D.ClassINET {
				alias := speedCheckAlias{strings.ToLower(cname.Hdr.Name), strings.ToLower(cname.Target)}
				if ttl, exists := aliasTTLs[alias]; !exists || cname.Hdr.Ttl < ttl {
					aliasTTLs[alias] = cname.Hdr.Ttl
				}
			}
		}
	}
	for _, rr := range message.Answer {
		if cname, ok := rr.(*D.CNAME); ok && cname.Hdr.Class == D.ClassINET {
			alias := speedCheckAlias{strings.ToLower(cname.Hdr.Name), strings.ToLower(cname.Target)}
			if ttl, exists := aliasTTLs[alias]; exists && ttl < cname.Hdr.Ttl {
				cname.Hdr.Ttl = ttl
			}
		}
	}
	return message
}

func speedCheckTerminalAddress(rr D.RR, owner string, qtype uint16) bool {
	return rr.Header().Class == D.ClassINET && rr.Header().Rrtype == qtype && strings.EqualFold(rr.Header().Name, owner)
}

// Unmeasured fallback and fastest-response answers retain upstream order. The
// count is still an upper bound; it is never a reason to invent reachability.
func speedCheckLimitAnswer(original *D.Msg, question D.Question, limit int) *D.Msg {
	chain, valid := speedCheckResponseChain(original, question)
	if !valid || speedCheckProtected(original) {
		return original
	}
	if limit == 0 {
		limit = DefaultMaxReplyIPNum
	}
	var records []D.RR
	seen := make(map[netip.Addr]D.RR)
	for _, rr := range original.Answer {
		if !speedCheckTerminalAddress(rr, chain.owner, question.Qtype) {
			continue
		}
		ip, valid := speedCheckAddress(rr)
		if !valid {
			continue
		}
		if previous := seen[ip]; previous != nil {
			if rr.Header().Ttl < previous.Header().Ttl {
				previous.Header().Ttl = rr.Header().Ttl
			}
		} else if len(records) < limit {
			copy := D.Copy(rr)
			records = append(records, copy)
			seen[ip] = copy
		}
	}
	if len(records) == 0 {
		return original
	}
	message := original.Copy()
	message.Answer = speedCheckReplaceAddresses(message.Answer, chain.owner, question.Qtype, records)
	return message
}

func speedCheckReplaceAddresses(answer []D.RR, owner string, qtype uint16, addresses []D.RR) []D.RR {
	result := make([]D.RR, 0, len(answer)+len(addresses))
	inserted := false
	for _, rr := range answer {
		if speedCheckTerminalAddress(rr, owner, qtype) {
			if !inserted {
				result = append(result, addresses...)
				inserted = true
			}
			continue
		}
		result = append(result, rr)
	}
	return result
}

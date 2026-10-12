package dns

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"net/netip"
	"time"

	D "github.com/miekg/dns"
)

// Repeated addresses from independent valid answers are useful when probing
// yields no measurements. Count each upstream answer once per address and
// never combine votes across incompatible alias paths. With no agreement,
// retain the normal first valid answer instead of inventing reachability.
func speedCheckUnmeasuredAnswer(ctx context.Context, first *D.Msg, question D.Question, responses []*D.Msg, limit int) *D.Msg {
	type vote struct {
		candidate speedCheckCandidate
		count     int
	}
	type voteKey struct {
		chain [32]byte
		ip    netip.Addr
	}
	votes := make(map[voteKey]*vote)
	var best *vote
	for _, response := range responses {
		chain, valid := speedCheckResponseChain(response, question)
		if !valid || speedCheckProtected(response) {
			continue
		}
		var identity []byte
		for _, link := range chain.links {
			for _, name := range []string{link.owner, link.target} {
				identity = binary.BigEndian.AppendUint32(identity, uint32(len(name)))
				identity = append(identity, name...)
			}
		}
		identity = append(identity, chain.owner...)
		chainKey := sha256.Sum256(identity)
		seen := make(map[netip.Addr]bool)
		for _, candidate := range speedCheckCandidates(response, question) {
			if seen[candidate.ip] {
				continue
			}
			seen[candidate.ip] = true
			key := voteKey{chain: chainKey, ip: candidate.ip}
			current := votes[key]
			if current == nil {
				if len(votes) >= maxSpeedCheckCandidates {
					continue
				}
				current = &vote{candidate: candidate}
				votes[key] = current
			}
			current.count++
			if best == nil || current.count > best.count {
				best = current
			}
		}
	}
	if best != nil && best.count >= 2 {
		// A zero score is local to this selection helper, not a successful
		// probe: it is never published as RTT or address-family evidence.
		return speedCheckAnswer(ctx, speedCheckResult{candidate: best.candidate}, question, responses,
			map[netip.Addr]time.Duration{best.candidate.ip: 0}, 1)
	}
	answer := speedCheckLimitAnswer(first, question, limit)
	inheritDNSAnswerLifetime(ctx, answer, first)
	return answer
}

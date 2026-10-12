package dns

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"time"
)

type speedCheckAttempt struct {
	ticket    *dnsProbeTicket
	candidate speedCheckCandidate
	stage     int
}

type speedCheckAttemptResult struct {
	id  uint64
	rtt time.Duration
	err error
}

// One query controls its stages; only an individual IP+mode probe is shared.
// All fields except the buffered results channel belong to the exchange loop.
type speedCheckStages struct {
	checker    *directSpeedChecker
	ctx        context.Context
	probe      speedCheckProbe
	results    chan speedCheckAttemptResult
	attempts   map[uint64]speedCheckAttempt
	candidates map[netip.Addr]speedCheckCandidate
	order      []netip.Addr
	serial     uint64
	stage      int
	measured   bool
	admitted   bool
	blocked    bool
	skipped    bool
}

func newSpeedCheckStages(ctx context.Context, checker *directSpeedChecker, probe speedCheckProbe) *speedCheckStages {
	return &speedCheckStages{
		checker: checker, ctx: ctx, probe: probe,
		results:  make(chan speedCheckAttemptResult, maxSpeedCheckCandidates*maxSpeedCheckModes),
		attempts: make(map[uint64]speedCheckAttempt), candidates: make(map[netip.Addr]speedCheckCandidate),
	}
}

func (s *speedCheckStages) add(candidate speedCheckCandidate) {
	if _, seen := s.candidates[candidate.ip]; seen || len(s.candidates) >= maxSpeedCheckCandidates {
		return
	}
	s.candidates[candidate.ip] = candidate
	s.order = append(s.order, candidate.ip)
	s.start(candidate)
}

func (s *speedCheckStages) canAdvance() bool {
	return !s.measured && !s.blocked && s.admitted && len(s.order) != 0 && s.stage+1 < len(s.checker.modes)
}

func (s *speedCheckStages) advance() {
	if !s.canAdvance() || s.ctx.Err() != nil {
		return
	}
	s.stage++
	for _, ip := range s.order {
		s.start(s.candidates[ip])
	}
	if len(s.attempts) == 0 {
		// Another query owns the remaining workers. Preserve the valid DNS
		// answer instead of creating a backlog behind unrelated requests.
		s.blocked = true
	}
}

func (s *speedCheckStages) start(candidate speedCheckCandidate) {
	s.serial++
	id := s.serial
	mode := s.checker.modes[s.stage]
	key := dnsProbeScope(s.ctx, candidate.ip)
	if key != "" {
		key += "|mode:" + mode.String()
	}
	work := func(ctx context.Context) (time.Duration, error) {
		return s.checker.checkMode(ctx, candidate.ip, mode, s.probe)
	}
	deliver := func(rtt time.Duration, err error) { s.results <- speedCheckAttemptResult{id: id, rtt: rtt, err: err} }
	ticket, err := s.checker.pool.submit(s.ctx, key, work, deliver)
	if errors.Is(err, errDNSProbeBusy) && s.stage > 0 {
		// A full pool must not make a blackholed first mode consume every
		// stage. Cancel only our old waiters; other queries keep their work.
		for _, old := range s.oldAttempts() {
			attempt := s.attempts[old]
			if attempt.ticket.cancelWaiter() {
				delete(s.attempts, old)
			}
			ticket, err = s.checker.pool.submit(s.ctx, key, work, deliver)
			if !errors.Is(err, errDNSProbeBusy) {
				break
			}
		}
	}
	if err != nil {
		s.skipped = true
		return
	}
	s.admitted = true
	s.attempts[id] = speedCheckAttempt{ticket: ticket, candidate: candidate, stage: s.stage}
}

func (s *speedCheckStages) oldAttempts() []uint64 {
	var ids []uint64
	for id, attempt := range s.attempts {
		if attempt.stage < s.stage && attempt.ticket != nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (s *speedCheckStages) record(result speedCheckAttemptResult) (speedCheckResult, bool) {
	attempt, waiting := s.attempts[result.id]
	if !waiting {
		return speedCheckResult{}, false
	}
	delete(s.attempts, result.id)
	if result.err == nil && result.rtt >= 0 {
		s.measured = true
	}
	return speedCheckResult{candidate: attempt.candidate, rtt: result.rtt, err: result.err}, true
}

func (s *speedCheckStages) close() {
	for id, attempt := range s.attempts {
		attempt.ticket.Release()
		delete(s.attempts, id)
	}
}

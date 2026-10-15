package dnsstats

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

type UpstreamResult uint8

const (
	UpstreamSuccess UpstreamResult = iota
	UpstreamError
	UpstreamCanceled
	UpstreamTimeout
	UpstreamRCodeError
)

type UpstreamStats struct {
	ID                   string  `json:"id"`
	Address              string  `json:"address"`
	Outbound             string  `json:"outbound"`
	OutboundType         string  `json:"outbound_type"`
	Attempts             uint64  `json:"attempts"`
	Successes            uint64  `json:"successes"`
	Errors               uint64  `json:"errors"`
	Canceled             uint64  `json:"canceled"`
	Timeouts             uint64  `json:"timeouts"`
	RCodeErrors          uint64  `json:"rcode_errors"`
	ElapsedMSAvg         float64 `json:"elapsed_ms_avg"`
	SuccessElapsedMSAvg  float64 `json:"success_elapsed_ms_avg"`
	ErrorElapsedMSAvg    float64 `json:"error_elapsed_ms_avg"`
	CanceledElapsedMSAvg float64 `json:"canceled_elapsed_ms_avg"`
}
type upstream struct {
	stats                UpstreamStats
	completed            uint64
	milliseconds         float64
	successMilliseconds  float64
	errorMilliseconds    float64
	canceledMilliseconds float64
}

// Outbound identifies an already selected physical exit. Key is opaque and is
// only hashed; it must never contain a rule/group identity in place of a leaf.
// An empty key means that the actual exit is unknown, not implicitly DIRECT.
type Outbound struct {
	Key  string
	Name string
	Type string
}

func normalizeOutbound(outbound Outbound) Outbound {
	if outbound.Key == "" || (outbound.Type != "direct" && outbound.Type != "proxy") {
		return Outbound{Type: "unknown"}
	}
	outbound.Name = bounded(outbound.Name, 128)
	return outbound
}

func NewUpstreamSource(address string, outbound Outbound) UpstreamSource {
	outbound = normalizeOutbound(outbound)
	return UpstreamSource{Address: sanitizeAddress(address), Outbound: outbound.Name, OutboundType: outbound.Type}
}

type Upstreams struct {
	Items        []UpstreamStats `json:"items"`
	Scope        string          `json:"scope"`
	AttemptScope string          `json:"attempt_scope"`
	Limit        int             `json:"limit"`
	Overflowed   uint64          `json:"overflowed"`
	InstanceID   string          `json:"instance_id"`
}

// Endpoint identity and the opaque outbound key are hashed; credentials,
// fragments and URL parameters never appear in API output. A custom DoH path
// may itself be a token, so expose only the conventional /dns-query path.
// Distinct hidden identities retain distinct IDs even when their sanitized
// display addresses are the same.
func sanitizeAddress(address string) string {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "unknown"
	}
	value := parsed.Scheme + "://" + parsed.Host
	if parsed.Path == "/dns-query" {
		value += "/dns-query"
	} else if parsed.Path != "" && parsed.Path != "/" {
		value += "/<redacted>"
	}
	return bounded(value, 256)
}

type Attempt struct {
	scope    scope
	key      [32]byte
	start    time.Time
	finished atomic.Bool
}

func StartUpstream(ctx context.Context, address string, exits ...Outbound) *Attempt {
	s := contextScope(ctx)
	if !s.manager.Enabled() {
		return nil
	}
	outbound := Outbound{Type: "unknown"}
	if len(exits) != 0 {
		outbound = normalizeOutbound(exits[0])
	}
	// Length-prefix the endpoint so it cannot collide with an opaque exit key.
	identity := make([]byte, 8, 8+len(address)+len(outbound.Type)+1+len(outbound.Key))
	binary.BigEndian.PutUint64(identity, uint64(len(address)))
	identity = append(identity, address...)
	identity = append(identity, outbound.Type...)
	identity = append(identity, 0)
	identity = append(identity, outbound.Key...)
	key := sha256.Sum256(identity)
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled.Load() || m.generation != s.generation {
		return nil
	}
	value := m.upstreams[key]
	if value == nil {
		if len(m.upstreams) >= UpstreamLimit {
			key = [32]byte{}
			value = m.upstreams[key]
			m.overflowed++
			if value == nil {
				value = &upstream{stats: UpstreamStats{ID: "overflow", Address: "other upstreams", OutboundType: "unknown"}}
				m.upstreams[key] = value
			}
		} else {
			value = &upstream{stats: UpstreamStats{ID: hex.EncodeToString(key[:]), Address: sanitizeAddress(address), Outbound: outbound.Name, OutboundType: outbound.Type}}
			m.upstreams[key] = value
		}
	}
	value.stats.Attempts++
	return &Attempt{scope: s, key: key, start: time.Now()}
}

func (a *Attempt) Complete(result UpstreamResult) {
	if a == nil {
		return
	}
	// End the exchange clock before waiting for observers/readers of the
	// statistics mutex. Their work is not part of this upstream's latency.
	elapsedMS := float64(time.Since(a.start)) / float64(time.Millisecond)
	if a.finished.Swap(true) {
		return
	}
	m := a.scope.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled.Load() || m.generation != a.scope.generation {
		return
	}
	value := m.upstreams[a.key]
	if value == nil {
		return
	}
	value.completed++
	value.milliseconds += elapsedMS
	switch result {
	case UpstreamSuccess:
		value.stats.Successes++
		value.successMilliseconds += elapsedMS
	case UpstreamCanceled:
		value.stats.Canceled++
		value.canceledMilliseconds += elapsedMS
	case UpstreamTimeout:
		value.stats.Errors++
		value.stats.Timeouts++
		value.errorMilliseconds += elapsedMS
	case UpstreamRCodeError:
		value.stats.Errors++
		value.stats.RCodeErrors++
		value.errorMilliseconds += elapsedMS
	default:
		value.stats.Errors++
		value.errorMilliseconds += elapsedMS
	}
}

func (m *Manager) Upstreams() Upstreams {
	m.mu.Lock()
	defer m.mu.Unlock()
	response := Upstreams{Items: make([]UpstreamStats, 0, len(m.upstreams)), Scope: "process", AttemptScope: "resolver_exchange", Limit: UpstreamLimit, Overflowed: m.overflowed, InstanceID: m.instance}
	for _, value := range m.upstreams {
		stats := value.stats
		if value.completed != 0 {
			stats.ElapsedMSAvg = value.milliseconds / float64(value.completed)
		}
		if stats.Successes != 0 {
			stats.SuccessElapsedMSAvg = value.successMilliseconds / float64(stats.Successes)
		}
		if stats.Errors != 0 {
			stats.ErrorElapsedMSAvg = value.errorMilliseconds / float64(stats.Errors)
		}
		if stats.Canceled != 0 {
			stats.CanceledElapsedMSAvg = value.canceledMilliseconds / float64(stats.Canceled)
		}
		response.Items = append(response.Items, stats)
	}
	sort.Slice(response.Items, func(i, j int) bool {
		if response.Items[i].Attempts == response.Items[j].Attempts {
			return strings.Compare(response.Items[i].ID, response.Items[j].ID) < 0
		}
		return response.Items[i].Attempts > response.Items[j].Attempts
	})
	return response
}

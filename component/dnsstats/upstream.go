package dnsstats

import (
	"context"
	"crypto/sha256"
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
	ID           string  `json:"id"`
	Address      string  `json:"address"`
	Attempts     uint64  `json:"attempts"`
	Successes    uint64  `json:"successes"`
	Errors       uint64  `json:"errors"`
	Canceled     uint64  `json:"canceled"`
	Timeouts     uint64  `json:"timeouts"`
	RCodeErrors  uint64  `json:"rcode_errors"`
	ElapsedMSAvg float64 `json:"elapsed_ms_avg"`
}
type upstream struct {
	stats        UpstreamStats
	completed    uint64
	milliseconds float64
}
type Upstreams struct {
	Items        []UpstreamStats `json:"items"`
	Scope        string          `json:"scope"`
	AttemptScope string          `json:"attempt_scope"`
	Limit        int             `json:"limit"`
	Overflowed   uint64          `json:"overflowed"`
	InstanceID   string          `json:"instance_id"`
}

// Only endpoint identity is hashed; credentials, fragments and URL parameters
// never appear in API output. A custom DoH path may itself be a token, so expose
// only the conventional /dns-query path. Distinct hidden identities retain
// distinct IDs even when their sanitized display addresses are the same.
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

func StartUpstream(ctx context.Context, address string) *Attempt {
	s := contextScope(ctx)
	if !s.manager.Enabled() {
		return nil
	}
	key := sha256.Sum256([]byte(address))
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
				value = &upstream{stats: UpstreamStats{ID: "overflow", Address: "other upstreams"}}
				m.upstreams[key] = value
			}
		} else {
			value = &upstream{stats: UpstreamStats{ID: hex.EncodeToString(key[:]), Address: sanitizeAddress(address)}}
			m.upstreams[key] = value
		}
	}
	value.stats.Attempts++
	return &Attempt{scope: s, key: key, start: time.Now()}
}

func (a *Attempt) Complete(result UpstreamResult) {
	if a == nil || a.finished.Swap(true) {
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
	value.milliseconds += float64(time.Since(a.start)) / float64(time.Millisecond)
	switch result {
	case UpstreamSuccess:
		value.stats.Successes++
	case UpstreamCanceled:
		value.stats.Canceled++
	case UpstreamTimeout:
		value.stats.Errors++
		value.stats.Timeouts++
	case UpstreamRCodeError:
		value.stats.Errors++
		value.stats.RCodeErrors++
	default:
		value.stats.Errors++
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

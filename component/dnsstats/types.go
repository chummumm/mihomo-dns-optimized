// Package dnsstats keeps bounded, process-local DNS observations. It does not
// import the resolver or write files, and its context values are observational.
package dnsstats

import (
	"context"
	"crypto/rand"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	Capacity         = 4096
	MemoryLimitBytes = 8 << 20
	Retention        = 24 * time.Hour
	UpstreamLimit    = 128
	MaxAnswers       = 8
	MaxAnswerBytes   = 256
)

type Outcome uint32

const (
	Local Outcome = iota
	CacheFresh
	CacheStale
	Hosts
	FakeIP
	Upstream
	Reject
	Drop
	Error
	outcomeCount
)

var outcomeNames = [...]string{"local", "cache_fresh", "cache_stale", "hosts", "fake_ip", "upstream", "reject", "drop", "error"}

func (o Outcome) String() string {
	if o >= outcomeCount {
		return "local"
	}
	return outcomeNames[o]
}

func ParseOutcome(value string) (Outcome, bool) {
	for i, name := range outcomeNames {
		if value == name {
			return Outcome(i), true
		}
	}
	return Local, false
}

type Answer struct {
	Type  string `json:"type"`
	Value string `json:"value"`
	TTL   uint32 `json:"ttl"`
}

type Record struct {
	ID               string    `json:"id"`
	Time             time.Time `json:"time"`
	QName            string    `json:"qname"`
	Client           string    `json:"client"`
	QType            string    `json:"qtype"`
	Protocol         string    `json:"protocol"`
	Source           string    `json:"source"`
	Outcome          string    `json:"outcome"`
	Cache            string    `json:"cache"`
	RCode            string    `json:"rcode"`
	ElapsedMS        float64   `json:"elapsed_ms"`
	Answers          []Answer  `json:"answers"`
	AnswersTruncated bool      `json:"answers_truncated"`
	Error            string    `json:"error"`
	Upstream         string    `json:"upstream"`
}

type Counts struct {
	Queries      uint64  `json:"queries"`
	CacheFresh   uint64  `json:"cache_fresh"`
	CacheStale   uint64  `json:"cache_stale"`
	Hosts        uint64  `json:"hosts"`
	FakeIP       uint64  `json:"fake_ip"`
	Upstream     uint64  `json:"upstream"`
	Reject       uint64  `json:"reject"`
	Drop         uint64  `json:"drop"`
	Errors       uint64  `json:"errors"`
	Local        uint64  `json:"local"`
	ElapsedMSAvg float64 `json:"elapsed_ms_avg"`
	ElapsedMSP95 float64 `json:"elapsed_ms_p95"`
}

type Point struct {
	Time         time.Time `json:"time"`
	Queries      uint64    `json:"queries"`
	CacheHits    uint64    `json:"cache_hits"`
	Errors       uint64    `json:"errors"`
	ElapsedMSAvg float64   `json:"elapsed_ms_avg"`
}

type Top struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type Stats struct {
	InstanceID string    `json:"instance_id"`
	StartedAt  time.Time `json:"started_at"`
	Totals     Counts    `json:"totals"`
	Last24H    Counts    `json:"last_24h"`
	Series     []Point   `json:"series"`
	TopDomains []Top     `json:"top_domains"`
	TopClients []Top     `json:"top_clients"`
	TopScope   string    `json:"top_scope"`
	Retained   int       `json:"retained"`
	Evicted    uint64    `json:"evicted"`
}

type Status struct {
	Version               int       `json:"version"`
	Enabled               bool      `json:"enabled"`
	Storage               string    `json:"storage"`
	InstanceID            string    `json:"instance_id"`
	StartedAt             time.Time `json:"started_at"`
	RetentionSeconds      int64     `json:"retention_seconds"`
	Capacity              int       `json:"capacity"`
	MemoryLimitBytes      int64     `json:"memory_limit_bytes"`
	MemoryBudgetKind      string    `json:"memory_budget_kind"`
	LatencyPercentileKind string    `json:"latency_percentile_kind"`
	AccountedBytes        int64     `json:"accounted_bytes"`
	Retained              int       `json:"retained"`
	Evicted               uint64    `json:"evicted"`
	OldestID              string    `json:"oldest_id"`
	NewestID              string    `json:"newest_id"`
	UpstreamLimit         int       `json:"upstream_limit"`
}

type Filter struct {
	Cursor                        uint64 // exclusive upper ID bound; zero starts at the newest record
	Limit                         int
	QName, Client, Outcome, QType string
}

type Page struct {
	Items      []Record `json:"items"`
	NextCursor string   `json:"next_cursor"`
	HasMore    bool     `json:"has_more"`
	Retained   int      `json:"retained"`
	Evicted    uint64   `json:"evicted"`
	Scope      string   `json:"scope"`
	InstanceID string   `json:"instance_id"`
}

type Source struct{ Client, Protocol, Name string }
type sourceKey struct{}
type scopeKey struct{}
type queryKey struct{}
type scope struct {
	manager    *Manager
	generation uint64
}

// WithSource supplies missing ingress information without affecting routing.
func WithSource(ctx context.Context, source Source) context.Context {
	return context.WithValue(ctx, sourceKey{}, source)
}

func SourceFromContext(ctx context.Context) Source {
	s, _ := ctx.Value(sourceKey{}).(Source)
	return s
}

// WithManager is useful for isolated consumers/tests. A scope survives shared
// work, but its generation cannot repopulate a disabled/re-enabled manager.
func WithManager(ctx context.Context, manager *Manager) context.Context {
	manager.mu.Lock()
	generation := manager.generation
	manager.mu.Unlock()
	return context.WithValue(ctx, scopeKey{}, scope{manager, generation})
}

func contextScope(ctx context.Context) scope {
	if s, ok := ctx.Value(scopeKey{}).(scope); ok {
		return s
	}
	Default.mu.Lock()
	s := scope{Default, Default.generation}
	Default.mu.Unlock()
	return s
}

type Query struct {
	scope    scope
	start    time.Time
	outcome  atomic.Uint32
	finished atomic.Bool
}

func (q *Query) Outcome() Outcome { return Outcome(q.outcome.Load()) }

func StartQuery(ctx context.Context) (context.Context, *Query) {
	s := contextScope(ctx)
	if !s.manager.Enabled() {
		return ctx, nil
	}
	q := &Query{scope: s, start: time.Now()}
	ctx = context.WithValue(ctx, scopeKey{}, s)
	return context.WithValue(ctx, queryKey{}, q), q
}

func MarkOutcome(ctx context.Context, outcome Outcome) {
	if q, _ := ctx.Value(queryKey{}).(*Query); q != nil && !q.finished.Load() {
		q.outcome.Store(uint32(outcome))
	}
}

// DetachQuery keeps the aggregate observer/generation but detaches the first
// caller's mutable result from shared, auxiliary and background work.
func DetachQuery(ctx context.Context) context.Context {
	return context.WithValue(ctx, queryKey{}, (*Query)(nil))
}

func (q *Query) Complete(record Record) {
	if q == nil || q.finished.Swap(true) {
		return
	}
	if record.Outcome == "" {
		record.Outcome = Outcome(q.outcome.Load()).String()
	}
	record.ElapsedMS = float64(time.Since(q.start)) / float64(time.Millisecond)
	q.scope.manager.record(record, q.scope.generation)
}

func NormalizedQName(value string) string {
	value = strings.ToLower(strings.TrimSuffix(value, "."))
	if value == "" {
		return "."
	}
	return bounded(value, 253)
}

func bounded(value string, limit int) string {
	if len(value) > limit {
		value = value[:limit]
	}
	// A one-byte replacement keeps invalid UTF-8 input within the byte cap.
	return strings.Clone(strings.ToValidUTF8(value, "?"))
}

func unknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return bounded(value, 64)
}

func sequence(value uint64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatUint(value, 10)
}

func instanceID() string { return rand.Text() }

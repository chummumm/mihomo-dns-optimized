package dnsstats

import (
	"container/heap"
	"math"
	"math/bits"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// The budget includes a conservative per-record allowance for ring entries,
// indexes and Go map overhead, plus fixed structures. It is not a process RSS
// promise: the allocator, API response copies and garbage collector are separate.
const recordOverhead = 1024
const fixedOverhead = 2 << 20
const minutes = 24 * 60

var Default = New()

type tally struct {
	outcomes     [outcomeCount]uint64
	cacheHits    [2]uint64
	histogram    [33]uint64 // logarithmic microsecond buckets, bounded upper estimates
	queries      uint64
	milliseconds float64
}

func (t *tally) add(outcome Outcome, elapsedMS float64, cache string) {
	t.queries++
	t.outcomes[outcome]++
	if cache == "fresh" {
		t.cacheHits[0]++
	} else if cache == "stale" {
		t.cacheHits[1]++
	}
	t.milliseconds += elapsedMS
	micros := uint64(math.Ceil(elapsedMS * 1000))
	bucket := 0
	if micros > 0 {
		bucket = bits.Len64(micros - 1)
	}
	if bucket >= len(t.histogram) {
		bucket = len(t.histogram) - 1
	}
	t.histogram[bucket]++
}

func (t *tally) merge(other tally) {
	t.queries += other.queries
	t.milliseconds += other.milliseconds
	for i := range t.cacheHits {
		t.cacheHits[i] += other.cacheHits[i]
	}
	for i := range t.outcomes {
		t.outcomes[i] += other.outcomes[i]
	}
	for i := range t.histogram {
		t.histogram[i] += other.histogram[i]
	}
}

func (t tally) counts() Counts {
	c := Counts{Queries: t.queries, CacheFresh: t.cacheHits[0], CacheStale: t.cacheHits[1],
		Hosts: t.outcomes[Hosts], FakeIP: t.outcomes[FakeIP], Upstream: t.outcomes[Upstream],
		Reject: t.outcomes[Reject], Drop: t.outcomes[Drop], Errors: t.outcomes[Error], Local: t.outcomes[Local]}
	if t.queries == 0 {
		return c
	}
	c.ElapsedMSAvg = t.milliseconds / float64(t.queries)
	target := t.queries - t.queries/20
	var seen uint64
	for i, value := range t.histogram {
		seen += value
		if seen >= target {
			c.ElapsedMSP95 = float64(uint64(1)<<i) / 1000
			break
		}
	}
	return c
}

type minute struct {
	epoch int64
	tally tally
}
type index struct {
	tail  uint64
	count int
}
type entry struct {
	record                       Record
	id                           uint64
	namePrevious, clientPrevious uint64
	cost                         int64
}

type Manager struct {
	mu             sync.Mutex
	enabled        atomic.Bool
	generation     uint64
	instance       string
	started        time.Time
	now            func() time.Time
	capacity       int
	budget         int64
	entries        []entry
	oldest, newest uint64
	count          int
	accounted      int64
	evicted        uint64
	names, clients map[string]index
	totals         tally
	buckets        *[minutes]minute
	upstreams      map[[32]byte]*upstream
	overflowed     uint64
}

func New() *Manager { return newManager(Capacity, MemoryLimitBytes, time.Now) }

func newManager(capacity int, budget int64, now func() time.Time) *Manager {
	m := &Manager{capacity: capacity, budget: budget, now: now}
	m.Configure(true)
	return m
}

func (m *Manager) Enabled() bool { return m.enabled.Load() }

// Configure preserves observations for an ordinary enabled->enabled reload.
// Disabling releases all retained data; re-enabling starts a new epoch. Late
// completions from the old epoch are ignored even after collection resumes.
func (m *Manager) Configure(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enabled.Load() == enabled {
		return
	}
	m.enabled.Store(false)
	m.generation++
	m.entries, m.names, m.clients, m.buckets, m.upstreams = nil, nil, nil, nil, nil
	m.count, m.accounted, m.oldest, m.newest, m.evicted, m.overflowed = 0, 0, 0, 0, 0, 0
	m.totals = tally{}
	m.instance, m.started = instanceID(), m.now().UTC()
	if enabled {
		m.entries = make([]entry, m.capacity)
		m.names, m.clients = make(map[string]index), make(map[string]index)
		m.buckets = new([minutes]minute)
		m.upstreams = make(map[[32]byte]*upstream)
		m.accounted = fixedOverhead
	}
	m.enabled.Store(enabled)
}

func cleanRecord(record Record) (Record, Outcome, int64) {
	record.QName = NormalizedQName(record.QName)
	record.Client, record.Protocol, record.Source = unknown(record.Client), unknown(record.Protocol), unknown(record.Source)
	if client, err := netip.ParseAddr(record.Client); err == nil {
		record.Client = client.Unmap().String()
	}
	record.QType = bounded(record.QType, 16)
	record.RCode = bounded(record.RCode, 24)
	record.Error = bounded(record.Error, 128)
	// Query upstream attribution is deliberately optional; never retain an
	// arbitrary URL or an original transport/route object in a query record.
	if record.Upstream != "" {
		record.Upstream = sanitizeAddress(record.Upstream)
	}
	outcome, ok := ParseOutcome(record.Outcome)
	if !ok {
		outcome = Local
	}
	record.Outcome = outcome.String()
	if record.Cache != "fresh" && record.Cache != "stale" && record.Cache != "miss" && record.Cache != "none" {
		switch outcome {
		case CacheFresh:
			record.Cache = "fresh"
		case CacheStale:
			record.Cache = "stale"
		case Upstream:
			record.Cache = "miss"
		default:
			record.Cache = "none"
		}
	}
	if math.IsNaN(record.ElapsedMS) || math.IsInf(record.ElapsedMS, 0) || record.ElapsedMS < 0 {
		record.ElapsedMS = 0
	}
	// DNS work already has deadlines; cap accounting defensively for callers.
	if record.ElapsedMS > float64(time.Hour/time.Millisecond) {
		record.ElapsedMS = float64(time.Hour / time.Millisecond)
	}
	answers := record.Answers
	if len(answers) > MaxAnswers {
		answers = answers[:MaxAnswers]
		record.AnswersTruncated = true
	}
	record.Answers = make([]Answer, len(answers))
	cost := int64(recordOverhead + len(record.QName) + len(record.Client) + len(record.QType) + len(record.Protocol) + len(record.Source) + len(record.RCode) + len(record.Error) + len(record.Upstream))
	for i, answer := range answers {
		if len(answer.Value) > MaxAnswerBytes || !utf8.ValidString(answer.Value) {
			record.AnswersTruncated = true
		}
		record.Answers[i] = Answer{Type: bounded(answer.Type, 16), Value: bounded(answer.Value, MaxAnswerBytes), TTL: answer.TTL}
		cost += int64(32 + len(record.Answers[i].Type) + len(record.Answers[i].Value))
	}
	return record, outcome, cost
}

func (m *Manager) record(record Record, generation uint64) {
	if !m.Enabled() {
		return
	}
	record, outcome, cost := cleanRecord(record)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled.Load() || generation != m.generation {
		return
	}
	now := m.now().UTC()
	record.Time = now
	m.totals.add(outcome, record.ElapsedMS, record.Cache)
	epoch := now.Unix() / 60
	bucket := &m.buckets[epoch%minutes]
	if bucket.epoch != epoch {
		*bucket = minute{epoch: epoch}
	}
	bucket.tally.add(outcome, record.ElapsedMS, record.Cache)
	m.expire(now)
	if cost+fixedOverhead > m.budget {
		m.evicted++
		return
	}
	for m.count > 0 && (m.count == m.capacity || m.accounted+cost > m.budget) {
		m.evict()
	}
	m.newest++
	id := m.newest
	if m.count == 0 {
		m.oldest = id
	}
	record.ID = sequence(id)
	name, client := m.names[record.QName], m.clients[record.Client]
	m.entries[(id-1)%uint64(m.capacity)] = entry{record: record, id: id, namePrevious: name.tail, clientPrevious: client.tail, cost: cost}
	m.names[record.QName] = index{tail: id, count: name.count + 1}
	m.clients[record.Client] = index{tail: id, count: client.count + 1}
	m.count++
	m.accounted += cost
}

func (m *Manager) evict() {
	e := &m.entries[(m.oldest-1)%uint64(m.capacity)]
	for _, group := range []struct {
		key   string
		index map[string]index
	}{{e.record.QName, m.names}, {e.record.Client, m.clients}} {
		value := group.index[group.key]
		if value.count <= 1 {
			delete(group.index, group.key)
		} else {
			value.count--
			group.index[group.key] = value
		}
	}
	m.accounted -= e.cost
	*e = entry{}
	m.oldest++
	m.count--
	m.evicted++
}

func (m *Manager) expire(now time.Time) {
	cutoff := now.Add(-Retention)
	for m.count > 0 {
		if !m.entries[(m.oldest-1)%uint64(m.capacity)].record.Time.Before(cutoff) {
			break
		}
		m.evict()
	}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire(m.now())
	s := Status{Version: 1, Enabled: m.enabled.Load(), Storage: "memory", InstanceID: m.instance, StartedAt: m.started,
		RetentionSeconds: int64(Retention / time.Second), Capacity: m.capacity, MemoryLimitBytes: m.budget,
		MemoryBudgetKind: "bounded_accounting", LatencyPercentileKind: "logarithmic_upper_bound", AccountedBytes: m.accounted, Retained: m.count, Evicted: m.evicted, UpstreamLimit: UpstreamLimit}
	if m.count > 0 {
		s.OldestID, s.NewestID = sequence(m.oldest), sequence(m.newest)
	}
	return s
}

func (m *Manager) Queries(filter Filter) Page {
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.Limit > 200 {
		filter.Limit = 200
	}
	if filter.QName != "" {
		filter.QName = NormalizedQName(filter.QName)
	}
	filter.Client = bounded(filter.Client, 64)
	if client, err := netip.ParseAddr(filter.Client); err == nil {
		filter.Client = client.Unmap().String()
	}
	filter.QType = strings.ToUpper(filter.QType)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire(m.now())
	capacity := filter.Limit
	if m.count < capacity {
		capacity = m.count
	}
	page := Page{Items: []Record{}, Retained: m.count, Evicted: m.evicted, Scope: "retained", InstanceID: m.instance}
	if m.count == 0 {
		return page
	}
	id := m.newest
	indexKind := 0
	if filter.QName != "" {
		value := m.names[filter.QName]
		if value.count == 0 {
			return page
		}
		id, indexKind = value.tail, 1
		if value.count < capacity {
			capacity = value.count
		}
	}
	if filter.Client != "" {
		value := m.clients[filter.Client]
		if value.count == 0 {
			return page
		}
		if indexKind == 0 || value.count < m.names[filter.QName].count {
			id, indexKind = value.tail, 2
			if value.count < capacity {
				capacity = value.count
			}
		}
	}
	if indexKind == 0 && filter.Cursor != 0 {
		if filter.Cursor-1 < id {
			id = filter.Cursor - 1
		}
	}
	page.Items = make([]Record, 0, capacity)
	for id >= m.oldest && id != 0 {
		e := &m.entries[(id-1)%uint64(m.capacity)]
		if e.id != id {
			break
		}
		r := e.record
		match := (filter.Cursor == 0 || id < filter.Cursor) && (filter.QName == "" || r.QName == filter.QName) && (filter.Client == "" || r.Client == filter.Client) && (filter.Outcome == "" || r.Outcome == filter.Outcome) && (filter.QType == "" || r.QType == filter.QType)
		if match {
			if len(page.Items) == filter.Limit {
				page.HasMore = true
				page.NextCursor = page.Items[len(page.Items)-1].ID
				break
			}
			r.Answers = append([]Answer{}, r.Answers...)
			page.Items = append(page.Items, r)
		}
		switch indexKind {
		case 1:
			id = e.namePrevious
		case 2:
			id = e.clientPrevious
		default:
			id--
		}
	}
	return page
}

type topHeap []Top

func (h topHeap) Len() int { return len(h) }
func (h topHeap) Less(i, j int) bool {
	if h[i].Count == h[j].Count {
		return h[i].Name > h[j].Name
	}
	return h[i].Count < h[j].Count
}
func (h topHeap) Swap(i, j int)   { h[i], h[j] = h[j], h[i] }
func (h *topHeap) Push(value any) { *h = append(*h, value.(Top)) }
func (h *topHeap) Pop() any       { old := *h; v := old[len(old)-1]; *h = old[:len(old)-1]; return v }
func topTen(values map[string]index) []Top {
	h := make(topHeap, 0, 10)
	for name, value := range values {
		item := Top{Name: name, Count: value.count}
		if len(h) < 10 {
			heap.Push(&h, item)
		} else if item.Count > h[0].Count || (item.Count == h[0].Count && item.Name < h[0].Name) {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	result := make([]Top, len(h))
	for i := len(result) - 1; i >= 0; i-- {
		result[i] = heap.Pop(&h).(Top)
	}
	return result
}

func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	m.expire(now)
	stats := Stats{InstanceID: m.instance, StartedAt: m.started, Totals: m.totals.counts(), Series: make([]Point, 0, minutes),
		TopDomains: topTen(m.names), TopClients: topTen(m.clients), TopScope: "retained", Retained: m.count, Evicted: m.evicted}
	if m.buckets == nil {
		return stats
	}
	epoch := now.Unix() / 60
	var recent tally
	for at := epoch - minutes + 1; at <= epoch; at++ {
		point := Point{Time: time.Unix(at*60, 0).UTC()}
		bucket := m.buckets[at%minutes]
		if bucket.epoch == at {
			counts := bucket.tally.counts()
			point.Queries, point.CacheHits, point.Errors, point.ElapsedMSAvg = counts.Queries, counts.CacheFresh+counts.CacheStale, counts.Errors, counts.ElapsedMSAvg
			recent.merge(bucket.tally)
		}
		stats.Series = append(stats.Series, point)
	}
	stats.Last24H = recent.counts()
	return stats
}

package dnsstats

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"
)

func recordForTest(m *Manager, name, client string, outcome Outcome) {
	m.record(Record{QName: name, Client: client, QType: "A", Outcome: outcome.String(), ElapsedMS: 12,
		Answers: []Answer{{Type: "A", Value: "192.0.2.1", TTL: 60}}}, m.generation)
}

func TestDNSObservabilityRingIndexesAndIndependentTotals(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	m := newManager(4, MemoryLimitBytes, func() time.Time { return now })
	for _, r := range []struct{ name, client string }{{"A.EXAMPLE.", "192.0.2.1"}, {"b.example", "192.0.2.2"}, {"a.example", "192.0.2.2"}, {"c.example", "192.0.2.1"}, {"a.example", "192.0.2.1"}} {
		recordForTest(m, r.name, r.client, CacheFresh)
	}
	status := m.Status()
	if status.Retained != 4 || status.Evicted != 1 || status.OldestID != "2" || status.NewestID != "5" {
		t.Fatalf("unexpected ring status: %+v", status)
	}
	page := m.Queries(Filter{Limit: 1, QName: "A.Example."})
	if len(page.Items) != 1 || page.Items[0].ID != "5" || !page.HasMore || page.NextCursor != "5" {
		t.Fatalf("unexpected indexed page: %+v", page)
	}
	recordForTest(m, "new.example", "192.0.2.3", Upstream)
	older := m.Queries(Filter{Limit: 2, Cursor: 5, QName: "a.example"})
	if len(older.Items) != 1 || older.Items[0].ID != "3" || older.HasMore {
		t.Fatalf("new inserts disrupted cursor: %+v", older)
	}
	intersection := m.Queries(Filter{QName: "a.example", Client: "192.0.2.1"})
	if len(intersection.Items) != 1 || intersection.Items[0].ID != "5" {
		t.Fatalf("bad index intersection: %+v", intersection)
	}
	stats := m.Stats()
	if stats.Totals.Queries != 6 || stats.Last24H.Queries != 6 || stats.Totals.CacheFresh != 5 || stats.Retained != 4 {
		t.Fatalf("eviction changed cumulative counters: %+v", stats)
	}
	if stats.TopDomains[0].Name != "a.example" || stats.TopDomains[0].Count != 2 {
		t.Fatalf("incorrect retained top: %+v", stats.TopDomains)
	}
	if len(m.names) > 4 || len(m.clients) > 4 {
		t.Fatal("indexes retained evicted keys")
	}
	for i := 0; i < 1000; i++ {
		recordForTest(m, fmt.Sprintf("%d.example", i), fmt.Sprint(i), Local)
	}
	if len(m.names) != 4 || len(m.clients) != 4 {
		t.Fatal("index cardinality grew past ring capacity")
	}
}

func TestDNSObservabilityMemoryBudgetAndOwnership(t *testing.T) {
	m := newManager(10, fixedOverhead+8000, time.Now)
	answer := strings.Repeat("x", 10000)
	record := Record{QName: "example.com", Outcome: Upstream.String(), Answers: make([]Answer, 20)}
	for i := range record.Answers {
		record.Answers[i] = Answer{Type: "TXT", Value: answer, TTL: 120}
	}
	for i := 0; i < 20; i++ {
		m.record(record, m.generation)
	}
	status := m.Status()
	if status.AccountedBytes > status.MemoryLimitBytes || status.Retained >= 10 || status.Retained == 0 {
		t.Fatalf("budget not enforced: %+v", status)
	}
	if m.Stats().Totals.Queries != 20 {
		t.Fatal("budget eviction reduced query totals")
	}
	record.Answers[0].Value = "caller mutation"
	page := m.Queries(Filter{})
	if len(page.Items[0].Answers) != MaxAnswers || len(page.Items[0].Answers[0].Value) > MaxAnswerBytes || !page.Items[0].AnswersTruncated {
		t.Fatal("answer summaries are unbounded or mutable")
	}
	page.Items[0].Answers[0].Value = "API mutation"
	if m.Queries(Filter{}).Items[0].Answers[0].Value == "API mutation" {
		t.Fatal("API output aliases retained answers")
	}
	baseline := int64(unsafe.Sizeof(entry{}))*Capacity + int64(unsafe.Sizeof([minutes]minute{})) + int64(unsafe.Sizeof(Manager{}))
	t.Logf("fixed structures: %d bytes; conservative fixed budget: %d bytes", baseline, fixedOverhead)
	if baseline > fixedOverhead {
		t.Fatalf("fixed accounting allowance %d is below structures %d", fixedOverhead, baseline)
	}
}

func TestDNSObservabilityRetentionReloadAndGeneration(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	m := newManager(4, MemoryLimitBytes, func() time.Time { return now })
	recordForTest(m, "one.example", "", Hosts)
	id := m.Status().InstanceID
	m.Configure(true)
	if m.Status().InstanceID != id || m.Stats().Totals.Queries != 1 {
		t.Fatal("ordinary reload reset statistics")
	}
	now = now.Add(Retention + time.Minute)
	if m.Status().Retained != 0 || m.Stats().Last24H.Queries != 0 || m.Stats().Totals.Queries != 1 {
		t.Fatal("time retention corrupted independent totals")
	}
	ctx, q := StartQuery(WithManager(context.Background(), m))
	a := StartUpstream(ctx, "https://dns.example/dns-query")
	m.Configure(false)
	if m.entries != nil || m.names != nil || m.clients != nil || m.upstreams != nil || m.buckets != nil || m.Status().AccountedBytes != 0 {
		t.Fatal("disabled manager retained observation allocations")
	}
	m.Configure(true)
	q.Complete(Record{QName: "late.example"})
	a.Complete(UpstreamSuccess)
	if m.Stats().Totals.Queries != 0 || len(m.Upstreams().Items) != 0 || m.Status().InstanceID == id {
		t.Fatal("old generation repopulated enabled manager")
	}
	_, newQuery := StartQuery(WithManager(context.Background(), m))
	newQuery.Complete(Record{QName: "fresh.example"})
	newQuery.Complete(Record{QName: "duplicate.example"})
	if m.Stats().Totals.Queries != 1 {
		t.Fatal("new query was not counted exactly once")
	}
}

func TestDNSObservabilityDetachedWorkCannotOverwriteClient(t *testing.T) {
	m := New()
	ctx, q := StartQuery(WithManager(context.Background(), m))
	MarkOutcome(ctx, CacheStale)
	MarkOutcome(DetachQuery(ctx), Upstream)
	q.Complete(Record{QName: "example.com"})
	if m.Queries(Filter{}).Items[0].Outcome != "cache_stale" {
		t.Fatal("shared work changed caller outcome")
	}
}

func TestDNSObservabilityUpstreamIdentityRedactionAndOverflow(t *testing.T) {
	m := New()
	ctx := WithManager(context.Background(), m)
	for _, secret := range []string{"first", "second"} {
		StartUpstream(ctx, "https://user:password@dns.example/private-"+secret+"?token="+secret+"#secret").Complete(UpstreamSuccess)
	}
	first := m.Upstreams()
	if len(first.Items) != 2 || first.Items[0].ID == first.Items[1].ID || first.Items[0].Address != first.Items[1].Address {
		t.Fatal("redaction merged distinct upstream identities")
	}
	encoded, _ := json.Marshal(first)
	for _, secret := range []string{"user", "password", "private-first", "private-second", "token", "secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("API exposed endpoint credential: %s", encoded)
		}
	}
	for i := 0; i < 300; i++ {
		StartUpstream(ctx, fmt.Sprintf("udp://192.0.2.1:%d", i+1000)).Complete(UpstreamCanceled)
	}
	stats := m.Upstreams()
	var attempts, canceled uint64
	for _, value := range stats.Items {
		attempts += value.Attempts
		canceled += value.Canceled
	}
	if len(stats.Items) != UpstreamLimit+1 || attempts != 302 || canceled != 300 || stats.Overflowed != 174 {
		t.Fatalf("bad upstream cardinality/totals: %+v", stats)
	}
	if m.Stats().Totals.Queries != 0 {
		t.Fatal("internal upstream work counted as client queries")
	}
}

func TestDNSObservabilityConcurrentReadersWriters(t *testing.T) {
	m := newManager(128, MemoryLimitBytes, time.Now)
	var group sync.WaitGroup
	for g := 0; g < 8; g++ {
		group.Add(1)
		go func(g int) {
			defer group.Done()
			for i := 0; i < 200; i++ {
				ctx, q := StartQuery(WithManager(context.Background(), m))
				MarkOutcome(ctx, CacheFresh)
				q.Complete(Record{QName: fmt.Sprintf("%d.example", g), Client: fmt.Sprint(g)})
				if i%20 == 0 {
					_ = m.Queries(Filter{QName: fmt.Sprintf("%d.example", g)})
					_ = m.Stats()
					_ = m.Status()
				}
			}
		}(g)
	}
	group.Wait()
	if m.Stats().Totals.Queries != 1600 || m.Status().Retained != 128 {
		t.Fatal("concurrent accounting lost records or totals")
	}
}

func TestDNSObservabilityEmptyArraysAndApproximatePercentile(t *testing.T) {
	m := New()
	for _, value := range []any{m.Queries(Filter{}), m.Stats(), m.Upstreams()} {
		encoded, err := json.Marshal(value)
		if err != nil || strings.Contains(string(encoded), ":null") {
			t.Fatalf("API emitted nullable collection: %s, %v", encoded, err)
		}
	}
	for i := 0; i < 100; i++ {
		m.record(Record{QName: "a", ElapsedMS: 10}, m.generation)
	}
	p95 := m.Stats().Totals.ElapsedMSP95
	if p95 < 10 || p95 > 20 || m.Status().LatencyPercentileKind != "logarithmic_upper_bound" {
		t.Fatalf("bad approximate percentile: %v", p95)
	}
}

func TestDNSObservabilityClientFilterNormalizesEquivalentIP(t *testing.T) {
	m := New()
	recordForTest(m, "v6.example", "2001:db8::1", Upstream)
	recordForTest(m, "v4.example", "192.0.2.1", Upstream)
	for _, client := range []string{"2001:DB8::1", "2001:db8:0:0:0:0:0:1", "::ffff:192.0.2.1"} {
		if page := m.Queries(Filter{Client: client}); len(page.Items) != 1 {
			t.Fatalf("equivalent client %q missed index: %+v", client, page)
		}
	}
}

func TestDNSObservabilityFieldBoundsWithInvalidUTF8(t *testing.T) {
	for _, test := range []struct {
		value string
		limit int
	}{{"x€y", 2}, {strings.Repeat("a\xff", 10000), 256}, {strings.Repeat("界", 10000), 253}, {strings.Repeat("\xff", 10000), 64}} {
		got := bounded(test.value, test.limit)
		if len(got) > test.limit || !utf8.ValidString(got) {
			t.Fatalf("bounded output is invalid or too long: %q (%d/%d)", got, len(got), test.limit)
		}
	}
	m := New()
	m.record(Record{QName: "utf8.example", Answers: []Answer{{Type: "TXT", Value: "a\xffb"}, {Type: "TXT", Value: strings.Repeat("界", 1000)}}}, m.generation)
	record := m.Queries(Filter{}).Items[0]
	if !record.AnswersTruncated {
		t.Fatal("changed answer summary was not marked truncated")
	}
	for _, answer := range record.Answers {
		if len(answer.Value) > MaxAnswerBytes || !utf8.ValidString(answer.Value) {
			t.Fatal("answer output violated UTF8/byte bounds")
		}
	}
}

func BenchmarkDNSObservabilityRecord(b *testing.B) {
	m := New()
	record := Record{QName: "benchmark.example", Client: "192.0.2.1", QType: "A", Outcome: CacheFresh.String(), Answers: []Answer{{Type: "A", Value: "192.0.2.19", TTL: 30}}}
	for i := 0; i < Capacity; i++ {
		m.record(record, m.generation)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.record(record, m.generation)
	}
}

func BenchmarkDNSObservabilityIndexedQuery(b *testing.B) {
	for _, size := range []int{100, 4096} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			m := New()
			for i := 0; i < size; i++ {
				recordForTest(m, fmt.Sprintf("%d.example", i), "192.0.2.1", Upstream)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = m.Queries(Filter{QName: "0.example", Limit: 50})
			}
		})
	}
}

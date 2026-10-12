package dnsstats

import (
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func pageIDs(page Page) []string {
	ids := make([]string, len(page.Items))
	for i, item := range page.Items {
		ids[i] = item.ID
	}
	return ids
}

func queryIDForTest(t *testing.T, value string) uint64 {
	t.Helper()
	if value == "" {
		return 0
	}
	id, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func assertIndexedPages(t *testing.T, m *Manager, filter Filter, wantIDs, wantCursors []string, wantAnchor string) {
	t.Helper()
	first := m.Queries(filter)
	if first.Total != len(wantIDs) || first.AnchorID != wantAnchor || !reflect.DeepEqual(first.PageCursors, wantCursors) {
		t.Fatalf("wrong filtered history index: total=%d anchor=%q cursors=%v; want %d %q %v", first.Total, first.AnchorID, first.PageCursors, len(wantIDs), wantAnchor, wantCursors)
	}
	filter.Anchor = queryIDForTest(t, first.AnchorID)
	gotIDs := []string{}
	for i, cursor := range first.PageCursors {
		filter.Cursor = queryIDForTest(t, cursor)
		page := m.Queries(filter)
		if page.Total != first.Total || !reflect.DeepEqual(page.PageCursors, first.PageCursors) || page.AnchorID != first.AnchorID {
			t.Fatalf("cursor changed the history index on page %d: %+v", i+1, page)
		}
		if page.HasMore != (i+1 < len(first.PageCursors)) {
			t.Fatalf("wrong continuation on page %d: %+v", i+1, page)
		}
		if page.HasMore && page.NextCursor != first.PageCursors[i+1] {
			t.Fatalf("existing next_cursor disagrees with page index: %+v", page)
		}
		gotIDs = append(gotIDs, pageIDs(page)...)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("indexed pages lost, duplicated or reordered matches: got %v want %v", gotIDs, wantIDs)
	}
}

func TestDNSObservabilityPageIndexExactFilteredTotals(t *testing.T) {
	m := newManager(12, MemoryLimitBytes, time.Now)
	for i := 1; i <= 12; i++ {
		r := Record{QName: "other.example", Client: "192.0.2.2", QType: "A", Outcome: Upstream.String()}
		if i%3 == 0 {
			r.QName = "hit.example"
		}
		if i%2 == 0 {
			r.Client = "192.0.2.1"
		}
		if i%4 == 0 {
			r.QType = "AAAA"
		}
		if i%5 == 0 {
			r.Outcome = CacheFresh.String()
		}
		m.record(r, m.generation)
	}
	for _, test := range []struct {
		name    string
		filter  Filter
		ids     []string
		cursors []string
	}{
		{"all", Filter{Limit: 5}, []string{"12", "11", "10", "9", "8", "7", "6", "5", "4", "3", "2", "1"}, []string{"", "8", "3"}},
		{"domain-index", Filter{Limit: 2, QName: "HIT.Example."}, []string{"12", "9", "6", "3"}, []string{"", "9"}},
		{"client-index", Filter{Limit: 2, Client: "::ffff:192.0.2.1"}, []string{"12", "10", "8", "6", "4", "2"}, []string{"", "10", "6"}},
		{"intersect-indexes", Filter{Limit: 1, QName: "hit.example", Client: "192.0.2.1"}, []string{"12", "6"}, []string{"", "12"}},
		{"record-type", Filter{Limit: 2, QType: "aaaa"}, []string{"12", "8", "4"}, []string{"", "8"}},
		{"outcome", Filter{Limit: 1, Outcome: CacheFresh.String()}, []string{"10", "5"}, []string{"", "10"}},
		{"combined", Filter{Limit: 5, QName: "hit.example", Client: "192.0.2.1", QType: "AAAA", Outcome: Upstream.String()}, []string{"12"}, []string{""}},
		{"empty-filter", Filter{Limit: 5, QName: "missing.example"}, []string{}, []string{""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertIndexedPages(t, m, test.filter, test.ids, test.cursors, "12")
		})
	}
	// Existing callers may still use arbitrary exclusive cursors without anchors.
	legacy := m.Queries(Filter{Limit: 2, Cursor: 10})
	if !reflect.DeepEqual(pageIDs(legacy), []string{"9", "8"}) || legacy.NextCursor != "8" || !legacy.HasMore || legacy.Total != 12 {
		t.Fatalf("legacy cursor behavior changed: %+v", legacy)
	}
}

func TestDNSObservabilityPageIndexAnchorExcludesInsertsAndTracksEviction(t *testing.T) {
	m := newManager(8, MemoryLimitBytes, time.Now)
	for i := 1; i <= 8; i++ {
		name := "other.example"
		if i%2 == 0 {
			name = "hit.example"
		}
		recordForTest(m, name, "", Upstream)
	}
	filter := Filter{Limit: 2, QName: "hit.example", Anchor: 8}
	assertIndexedPages(t, m, filter, []string{"8", "6", "4", "2"}, []string{"", "6"}, "8")
	for i := 0; i < 2; i++ {
		recordForTest(m, "hit.example", "", Upstream)
	}
	assertIndexedPages(t, m, filter, []string{"8", "6", "4"}, []string{"", "6"}, "8")
	// Latest follows the new upper bound; history keeps the same first page.
	assertIndexedPages(t, m, Filter{Limit: 2, QName: "hit.example"}, []string{"10", "9", "8", "6", "4"}, []string{"", "9", "6"}, "10")
	for i := 0; i < 4; i++ {
		recordForTest(m, "hit.example", "", Upstream)
	}
	staleLast := m.Queries(Filter{Limit: 2, QName: "hit.example", Anchor: 8, Cursor: 6})
	if staleLast.Total != 1 || len(staleLast.Items) != 0 || !reflect.DeepEqual(staleLast.PageCursors, []string{""}) || staleLast.HasMore {
		t.Fatalf("evicted last page did not supply an exact smaller index: %+v", staleLast)
	}
	assertIndexedPages(t, m, filter, []string{"8"}, []string{""}, "8")
	for i := 0; i < 2; i++ {
		recordForTest(m, "hit.example", "", Upstream)
	}
	assertIndexedPages(t, m, filter, []string{}, []string{""}, "8")
}

func TestDNSObservabilityPageIndexTimeExpirationAndNewInstance(t *testing.T) {
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	m := newManager(8, MemoryLimitBytes, func() time.Time { return now })
	assertIndexedPages(t, m, Filter{Limit: 2}, []string{}, []string{""}, "")
	for i := 0; i < 4; i++ {
		recordForTest(m, "hit.example", "", Hosts)
	}
	oldInstance := m.Status().InstanceID
	now = now.Add(Retention + time.Second)
	assertIndexedPages(t, m, Filter{Limit: 2, Anchor: 4}, []string{}, []string{""}, "4")
	m.Configure(false)
	m.Configure(true)
	recordForTest(m, "new.example", "", Hosts)
	newPage := m.Queries(Filter{Limit: 2, Anchor: 4})
	if newPage.InstanceID == oldInstance || newPage.AnchorID != "1" || newPage.Total != 1 {
		t.Fatalf("new epoch did not carry its own identity/index: %+v", newPage)
	}
}

func TestDNSObservabilityPageIndexUint64IDsAndBounds(t *testing.T) {
	m := newManager(4, MemoryLimitBytes, time.Now)
	m.newest = ^uint64(0) - 3
	for i := 0; i < 3; i++ {
		recordForTest(m, fmt.Sprint(i), "", Upstream)
	}
	assertIndexedPages(t, m, Filter{Limit: 2}, []string{"18446744073709551615", "18446744073709551614", "18446744073709551613"}, []string{"", "18446744073709551614"}, "18446744073709551615")
	assertIndexedPages(t, m, Filter{Limit: 2, Anchor: ^uint64(0) - 1}, []string{"18446744073709551614", "18446744073709551613"}, []string{""}, "18446744073709551614")
	assertIndexedPages(t, m, Filter{Limit: 2, Anchor: 1}, []string{}, []string{""}, "1")
	bounded := New()
	for i := 0; i < Capacity; i++ {
		recordForTest(bounded, "all.example", "", Upstream)
	}
	if page := bounded.Queries(Filter{Limit: 50}); page.Total != Capacity || len(page.PageCursors) != 82 || len(page.Items) != 50 {
		t.Fatalf("normal browser page did not remain bounded: total=%d boundaries=%d rows=%d", page.Total, len(page.PageCursors), len(page.Items))
	}
}

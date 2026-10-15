package dnsstats

import (
	"context"
	"strings"
	"testing"
)

func TestDNSObservabilityQueryProvenanceIsBoundedAndOwned(t *testing.T) {
	m := New()
	ctx, query := StartQuery(WithManager(context.Background(), m))
	sources := make([]UpstreamSource, MaxQueryUpstreams+2)
	for i := range sources {
		sources[i] = UpstreamSource{Address: "https://user:secret@dns.example/token?key=secret", Outbound: strings.Repeat("x", 1000), OutboundType: "proxy"}
	}
	MarkUpstreams(ctx, sources)
	sources[0].Address = "caller mutation"
	query.Complete(Record{QName: "example", Outcome: "upstream", Upstreams: query.Upstreams()})
	page := m.Queries(Filter{})
	got := page.Items[0]
	if len(got.Upstreams) != MaxQueryUpstreams || !got.UpstreamsTruncated || got.Upstreams[0].Address != "https://dns.example/<redacted>" || len(got.Upstreams[0].Outbound) != 128 {
		t.Fatalf("unbounded or unsanitized query provenance: %+v", got)
	}
	page.Items[0].Upstreams[0].Outbound = "API mutation"
	if m.Queries(Filter{}).Items[0].Upstreams[0].Outbound == "API mutation" {
		t.Fatal("API response exposed retained provenance")
	}
}

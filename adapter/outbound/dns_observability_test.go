package outbound

import (
	"net/netip"
	"testing"

	"github.com/metacubex/mihomo/component/dnsstats"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
)

func TestDNSObservabilityExplicitOutboundDoesNotChangeRouting(t *testing.T) {
	metadata := &C.Metadata{SrcIP: netip.MustParseAddr("::ffff:192.0.2.7"), InName: "mixed"}
	for _, protocol := range []string{"tcp", "udp"} {
		ctx := dnsOutboundObservationContext(metadata, protocol)
		source := dnsstats.SourceFromContext(ctx)
		if source.Client != "192.0.2.7" || source.Protocol != protocol || source.Name != "mixed" {
			t.Fatalf("lost observed source: %+v", source)
		}
		if icontext.DNSRoutingMetadata(ctx) != nil || icontext.DNSRoutingInbound(ctx) {
			t.Fatal("observation changed explicit DNS outbound routing scope")
		}
	}
}

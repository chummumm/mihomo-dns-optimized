package context

import (
	"context"
	"net/netip"
	"testing"

	"github.com/metacubex/mihomo/common/contextutils"
	C "github.com/metacubex/mihomo/constant"
)

func TestDNSRuleRoutingContextIsolation(t *testing.T) {
	metadata := &C.Metadata{Host: "original.example", SrcIP: netip.MustParseAddr("192.0.2.1"), SpecialRules: "tenant-a", Process: "client"}
	ctx := WithDNSRoutingMetadata(context.Background(), metadata)
	metadata.Host = "caller-mutated.example"
	got := DNSRoutingMetadata(ctx)
	if got.Host != "original.example" || got.SpecialRules != "tenant-a" || got.Process != "client" {
		t.Fatalf("origin was not preserved: %+v", got)
	}
	got.Host = "reader-mutated.example"
	if DNSRoutingMetadata(ctx).Host != "original.example" {
		t.Fatal("readers share mutable metadata")
	}
	if DNSRoutingMetadata(context.Background()) != nil || DNSBootstrap(ctx) {
		t.Fatal("empty/default context has routing state")
	}
	bootstrap := WithDNSBootstrap(WithDNSFixedOutbound(ctx, nil))
	if !DNSBootstrap(bootstrap) || DNSFixedOutbound(bootstrap) != nil || DNSRoutingMetadata(bootstrap).SpecialRules != "tenant-a" {
		t.Fatal("bootstrap lost source metadata or nil fixed exit")
	}
	if !DNSBootstrap(contextutils.WithoutCancel(bootstrap)) {
		t.Fatal("background refresh lost bootstrap scope")
	}
	if DNSRoutingInbound(ctx) || !DNSRoutingInbound(contextutils.WithoutCancel(WithDNSRoutingInbound(ctx))) {
		t.Fatal("DNS inbound marker has the wrong scope")
	}
	fixed := &struct{ C.ProxyAdapter }{}
	if DNSFixedOutbound(WithDNSFixedOutbound(ctx, fixed)) != fixed || DNSFixedOutbound(WithDNSFixedOutbound(bootstrap, fixed)) != nil {
		t.Fatal("fixed business outbound leaked into bootstrap scope")
	}
}

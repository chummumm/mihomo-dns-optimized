package context

import (
	"context"

	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"

	"github.com/gofrs/uuid/v5"
)

type dnsRoutingMetadataKey struct{}
type dnsRoutingInboundKey struct{}
type dnsFixedOutboundKey struct{}
type dnsBootstrapKey struct{}

// WithDNSRoutingMetadata preserves the actual caller of an internal DNS query.
// It must never replace the DNS server address with the queried site's name.
func WithDNSRoutingMetadata(ctx context.Context, metadata *C.Metadata) context.Context {
	if metadata == nil {
		return ctx
	}
	return context.WithValue(ctx, dnsRoutingMetadataKey{}, metadata.Clone())
}

func DNSRoutingMetadata(ctx context.Context) *C.Metadata {
	if metadata, ok := ctx.Value(dnsRoutingMetadataKey{}).(*C.Metadata); ok {
		return metadata.Clone()
	}
	return nil
}

// WithDNSRoutingInbound distinguishes an actual DNS client request from a
// business connection whose target happens to need an internal DNS lookup.
func WithDNSRoutingInbound(ctx context.Context) context.Context {
	return context.WithValue(ctx, dnsRoutingInboundKey{}, true)
}

func DNSRoutingInbound(ctx context.Context) bool {
	value, _ := ctx.Value(dnsRoutingInboundKey{}).(bool)
	return value
}

// WithDNSFixedOutbound carries an already selected outbound into the local
// resolution of its business destination. Node/bootstrap lookup is separate.
func WithDNSFixedOutbound(ctx context.Context, outbound C.ProxyAdapter) context.Context {
	return context.WithValue(ctx, dnsFixedOutboundKey{}, struct{ outbound C.ProxyAdapter }{outbound})
}

func DNSFixedOutbound(ctx context.Context) C.ProxyAdapter {
	if DNSBootstrap(ctx) {
		return nil
	}
	if value, ok := ctx.Value(dnsFixedOutboundKey{}).(struct{ outbound C.ProxyAdapter }); ok {
		return value.outbound
	}
	return nil
}

// WithDNSBootstrap exempts the resolution needed to establish a proxy or DNS
// server connection from business-domain routing, preventing recursive setup.
func WithDNSBootstrap(ctx context.Context) context.Context {
	return context.WithValue(ctx, dnsBootstrapKey{}, true)
}

func DNSBootstrap(ctx context.Context) bool {
	value, _ := ctx.Value(dnsBootstrapKey{}).(bool)
	return value
}

const (
	DNSTypeHost   = "host"
	DNSTypeFakeIP = "fakeip"
	DNSTypeRaw    = "raw"
)

type DNSContext struct {
	context.Context

	id uuid.UUID
	tp string
}

func NewDNSContext(ctx context.Context) *DNSContext {
	return &DNSContext{
		Context: ctx,

		id: utils.NewUUIDV4(),
	}
}

// ID implement C.PlainContext ID
func (c *DNSContext) ID() uuid.UUID {
	return c.id
}

// SetType set type of response
func (c *DNSContext) SetType(tp string) {
	c.tp = tp
}

// Type return type of response
func (c *DNSContext) Type() string {
	return c.tp
}

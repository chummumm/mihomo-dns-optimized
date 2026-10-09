package dns

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"

	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/tunnel"
)

// A reply is reusable only AFTER this caller has passed the complete live rule
// evaluation. Keep source IP, process, authentication and inbound scopes; only
// an ephemeral source port may be omitted for entirely automatic, valid routes.
// Live exchanges use a stricter identity so API cancellation cannot cancel a
// different source-port's independent request. No routing decision is cached.
func makeDNSRouteKey(ctx context.Context, route *dnsQueryRoute, wire []byte, sharePort bool) string {
	m := route.origin
	b := make([]byte, 0, 512+len(wire))
	add := func(s string) {
		b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
		b = append(b, s...)
	}
	num := func(n uint64) { b = binary.BigEndian.AppendUint64(b, n) }
	add("dns-route-v2")
	if route.err != nil {
		add("error:" + route.err.Error())
	} else {
		add(route.plan.TransportKey())
	}
	add(route.poolIdentity())
	num(uint64(tunnel.Mode()))
	// Fixed adapters remain a distinct explicit execution context.
	if fixed := icontext.DNSFixedOutbound(ctx); fixed != nil {
		add(fmt.Sprintf("%T:%p", fixed, fixed))
	} else {
		add("")
	}
	num(uint64(m.NetWork))
	num(uint64(m.Type))
	num(uint64(m.DSCP))
	add(m.SrcIP.String())
	add(m.DstIP.String())
	add(m.InIP.String())
	if sharePort {
		num(0)
	} else {
		num(uint64(m.SrcPort))
	}
	num(uint64(m.DstPort))
	num(uint64(m.InPort))
	num(uint64(m.Uid))
	num(uint64(m.DNSMode))
	for _, s := range []string{m.SrcIPASN, m.DstIPASN, m.InName, m.InUser, m.RematchName,
		m.Host, m.Process, m.ProcessPath, m.SpecialProxy, m.SpecialRules, m.RemoteDst, m.SniffHost} {
		add(s)
	}
	for _, list := range [][]string{m.SrcGeoIP, m.DstGeoIP} {
		if list == nil {
			num(0)
		} else {
			num(uint64(len(list)) + 1)
		}
		for _, s := range list {
			add(s)
		}
	}
	// Pack produced a private buffer. Preserve every flag/EDNS byte, including
	// ECS, cookies, padding, DO/CD and case; only the transaction ID is local.
	wire[0], wire[1] = 0, 0
	b = append(b, wire...)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("dns-route-v2:%x", sum)
}

func dnsQueryFlightKey(ctx context.Context, key string) string {
	if route := queryRoute(ctx); route != nil && route.origin != nil {
		return key + "|source-port:" + strconv.Itoa(int(route.origin.SrcPort))
	}
	return key
}

var errDNSNativeBusy = errors.New("native DNS query capacity reached")
var dnsNativeWorkSlots = make(chan struct{}, 256)

// Admission is applied only to actual work in the fork's native DNS path, not
// to cache hits, bootstrap, ordinary traffic or existing upstream behavior.
func admitDNSNativeWork(ctx context.Context) (func(), error) {
	if queryRoute(ctx) == nil {
		return func() {}, nil
	}
	select {
	case dnsNativeWorkSlots <- struct{}{}:
		return func() { <-dnsNativeWorkSlots }, nil
	default:
		return nil, errDNSNativeBusy
	}
}

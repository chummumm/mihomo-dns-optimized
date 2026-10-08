package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"

	N "github.com/metacubex/mihomo/common/net"
	C "github.com/metacubex/mihomo/constant"

	"github.com/miekg/dns"
)

func TestDNSProxyResponseErrorsAndTruncation(t *testing.T) {
	request := new(dns.Msg).SetQuestion("Example.COM.", dns.TypeHTTPS)
	pack := func(message *dns.Msg) []byte {
		t.Helper()
		wire, err := message.Pack()
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	for _, rcode := range []int{dns.RcodeSuccess, dns.RcodeNameError, dns.RcodeServerFailure, dns.RcodeRefused, dns.RcodeFormatError, dns.RcodeNotImplemented, dns.RcodeBadVers, dns.RcodeBadCookie} {
		response := new(dns.Msg).SetRcode(request, rcode)
		if rcode > 15 {
			response.SetEdns0(1232, true)
		}
		if err := validateDNSProxyResponse(request, pack(response)); err != nil {
			t.Errorf("error code %d rejected: %v", rcode, err)
		}
	}
	for _, rcode := range []int{dns.RcodeFormatError, dns.RcodeRefused, dns.RcodeServerFailure, dns.RcodeNotImplemented} {
		response := new(dns.Msg).SetRcode(request, rcode)
		response.Question = nil
		wire := pack(response)
		if err := validateDNSProxyResponse(request, wire); err != nil {
			t.Errorf("header-only error %d rejected: %v", rcode, err)
		}
		badCount := append([]byte(nil), wire...)
		binary.BigEndian.PutUint16(badCount[4:6], 1)
		if validateDNSProxyResponse(request, badCount) == nil {
			t.Fatal("header-only error with missing question accepted")
		}
		response.Id++
		if validateDNSProxyResponse(request, pack(response)) == nil {
			t.Fatal("header-only error with wrong ID accepted")
		}
	}
	success := new(dns.Msg).SetReply(request)
	success.Question = nil
	if validateDNSProxyResponse(request, pack(success)) == nil {
		t.Fatal("header-only success accepted")
	}
	response := new(dns.Msg).SetReply(request)
	response.Truncated = true
	response.SetEdns0(1232, true)
	if err := validateDNSProxyResponse(request, pack(response)); err != nil {
		t.Fatalf("well-formed TC response rejected: %v", err)
	}
	response.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.IPv4(192, 0, 2, 1)}}
	response.Extra = nil
	wire := pack(response)
	if err := validateDNSProxyResponse(request, wire); err != nil {
		t.Fatalf("TC response containing complete records rejected: %v", err)
	}
	if validateDNSProxyResponse(request, wire[:len(wire)-1]) == nil {
		t.Fatal("physically incomplete RR accepted because TC was set")
	}
}

func TestDNSProxyUDPWaitsForMatchingResponse(t *testing.T) {
	query := dnsProxyTestQuery(t, "example.com.")
	good := dnsProxyTestReply(t, query)
	wrongID := append([]byte(nil), good...)
	wrongID[1] ^= 1
	wrongName := append([]byte(nil), good...)
	wrongName[13] = 'z'
	packets := &dnsProxyTestPacketConn{reads: make(chan dnsProxyTestDatagram, 4), writes: make(chan dnsProxyTestDatagram, 1), closed: make(chan struct{})}
	for _, wire := range [][]byte{wrongID, wrongName, []byte("not DNS"), good} {
		packets.reads <- dnsProxyTestDatagram{wire, &net.UDPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 53}}
	}
	base := &dnsProxyTestAdapter{dnsProxyTestBase: newDNSProxyTestBase("wire", C.Direct, true)}
	base.packet = func(context.Context, *C.Metadata) (C.PacketConn, error) {
		return &dnsProxyTestProxyPacketConn{EnhancePacketConn: N.NewEnhancePacketConn(packets)}, nil
	}
	got, err := exchangeDNSProxy(context.Background(), query, dnsProxyTestResolver(C.UDP), func(*C.Metadata) (dnsProxyRoute, error) {
		return dnsProxyRoute{proxy: newDNSProxyTestProxy(base)}, nil
	}, exchangeDNSProxyWire)
	if err != nil || !bytes.Equal(got, good) || len(packets.reads) != 0 {
		t.Fatalf("did not wait for the matching response: got %x, err %v, queued %d", got, err, len(packets.reads))
	}
}

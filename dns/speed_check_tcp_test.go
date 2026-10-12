package dns

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	D "github.com/miekg/dns"
)

func TestMeasureTCPConnectTreatsOnlyRefusalAsReachable(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{name: "connected"},
		{name: "refused", err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}},
		{name: "unreachable", err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}, want: syscall.ENETUNREACH},
		{name: "permission", err: syscall.EACCES, want: syscall.EACCES},
	} {
		t.Run(test.name, func(t *testing.T) {
			rtt, err := measureTCPConnect(context.Background(), func(context.Context) (net.Conn, error) {
				return nil, test.err
			})
			if !errors.Is(err, test.want) || (err == nil && rtt < 0) {
				t.Fatalf("TCP result: rtt=%v error=%v, want %v", rtt, err, test.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := measureTCPConnect(ctx, func(context.Context) (net.Conn, error) {
		cancel()
		return nil, syscall.ECONNREFUSED
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation became reachability evidence: %v", err)
	}
	client, server := net.Pipe()
	defer server.Close()
	if _, err := measureTCPConnect(context.Background(), func(context.Context) (net.Conn, error) { return client, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Write([]byte{1}); err == nil {
		t.Fatal("successful probe left its connection open")
	}
}

func TestTCPSYNChecksumVectors(t *testing.T) {
	for _, test := range []struct {
		source, destination string
		checksum            uint16
	}{
		{source: "192.0.2.10", destination: "198.51.100.20", checksum: 0x218f},
		{source: "2001:db8::10", destination: "2001:db8:1::20", checksum: 0xb23e},
	} {
		probe := tcpSYNProbe{
			source:      netip.AddrPortFrom(netip.MustParseAddr(test.source), 40000),
			destination: netip.AddrPortFrom(netip.MustParseAddr(test.destination), 443), sequence: 0x01020304,
		}
		wire := probe.request()
		if got := binary.BigEndian.Uint16(wire[16:18]); got != test.checksum {
			t.Errorf("%s checksum = %#04x, want %#04x", test.source, got, test.checksum)
		}
		if got := tcpProbeChecksum(probe.source.Addr(), probe.destination.Addr(), wire); got != 0 {
			t.Fatalf("%s request failed checksum verification: %#04x", test.source, got)
		}
	}
}

func TestTCPSYNReplyRequiresMatchingTupleAcknowledgementAndChecksum(t *testing.T) {
	for _, family := range []struct{ local, remote string }{
		{local: "192.0.2.10", remote: "198.51.100.20"},
		{local: "2001:db8::10", remote: "2001:db8:1::20"},
	} {
		t.Run(family.local, func(t *testing.T) {
			probe := tcpSYNProbe{
				source:      netip.AddrPortFrom(netip.MustParseAddr(family.local), 40000),
				destination: netip.AddrPortFrom(netip.MustParseAddr(family.remote), 443), sequence: ^uint32(0),
			}
			remote := tcpSYNProbe{source: probe.destination, destination: probe.source}
			for _, flags := range []byte{tcpFlagSYN | tcpFlagACK, tcpFlagRST | tcpFlagACK} {
				wire := remote.segment(12345, probe.sequence+1, flags)
				seq, synAck, valid := probe.reply(wire, probe.destination.Addr())
				if !valid || seq != 12345 || synAck != (flags&tcpFlagSYN != 0) {
					t.Fatalf("valid matching reply rejected: seq=%d SYN=%v valid=%v", seq, synAck, valid)
				}
			}
			for _, test := range []struct {
				name   string
				mutate func([]byte)
			}{
				{name: "source-port", mutate: func(wire []byte) { wire[1]++ }},
				{name: "destination-port", mutate: func(wire []byte) { wire[3]++ }},
				{name: "acknowledgement", mutate: func(wire []byte) { wire[11]++ }},
				{name: "bare-reset", mutate: func(wire []byte) { wire[13] = tcpFlagRST }},
				{name: "bare-ack", mutate: func(wire []byte) { wire[13] = tcpFlagACK }},
				{name: "syn-reset", mutate: func(wire []byte) { wire[13] |= tcpFlagRST }},
				{name: "fin", mutate: func(wire []byte) { wire[13] |= tcpFlagFIN }},
				{name: "short-header", mutate: func(wire []byte) { wire[12] = 4 << 4 }},
				{name: "oversized-header", mutate: func(wire []byte) { wire[12] = 15 << 4 }},
			} {
				t.Run(test.name, func(t *testing.T) {
					wire := remote.segment(12345, probe.sequence+1, tcpFlagSYN|tcpFlagACK)
					test.mutate(wire)
					// Repair the checksum to test the tuple/ACK/header check itself.
					wire[16], wire[17] = 0, 0
					binary.BigEndian.PutUint16(wire[16:18], tcpProbeChecksum(remote.source.Addr(), remote.destination.Addr(), wire))
					if _, _, valid := probe.reply(wire, probe.destination.Addr()); valid {
						t.Fatal("unrelated or malformed reply accepted")
					}
				})
			}
			wire := remote.segment(12345, probe.sequence+1, tcpFlagSYN|tcpFlagACK)
			if _, _, valid := probe.reply(wire, probe.source.Addr()); valid {
				t.Fatal("reply from unrelated address accepted")
			}
			if _, _, valid := probe.reply(wire[:19], probe.destination.Addr()); valid {
				t.Fatal("truncated header accepted")
			}
			wire[16]++
			if _, _, valid := probe.reply(wire, probe.destination.Addr()); valid {
				t.Fatal("corrupt checksum accepted")
			}
		})
	}
}

func TestSpeedCheckADOnlyQueryStillOptimizes(t *testing.T) {
	query := new(D.Msg).SetQuestion("ad-query.example.", D.TypeA)
	query.AuthenticatedData = true
	answer := speedCheckTestReply(t, query, "ad-query.example. 60 IN A 192.0.2.1", "ad-query.example. 60 IN A 192.0.2.2")
	answer.AuthenticatedData = false
	checker := speedCheckTestChecker(t, time.Second, 2)
	checker.probe = func(_ context.Context, ip netip.Addr, _ speedCheckMode) (time.Duration, error) {
		if ip == netip.MustParseAddr("192.0.2.2") {
			return time.Millisecond, nil
		}
		return time.Second, nil
	}
	got, _, err := checker.Exchange(context.Background(), []dnsClient{speedCheckTestClient{message: answer}}, query)
	if err != nil || len(got.Answer) != 1 || got.Answer[0].(*D.A).A.String() != "192.0.2.2" {
		t.Fatalf("AD query flag disabled optimization: answer=%v error=%v", got, err)
	}
	checker.candidates.store("ad-only", 0, query, answer)
	if cached, _ := checker.candidates.get("ad-only"); cached == nil {
		t.Fatal("AD-only query disabled safe raw candidate reuse")
	}
}

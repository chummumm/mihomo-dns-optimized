package dnsmessage

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func packMessage(t *testing.T, message *dns.Msg) []byte {
	t.Helper()
	wire, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestQueryTypesNamesAndExtensions(t *testing.T) {
	for _, typ := range []uint16{
		dns.TypeA, dns.TypeAAAA, dns.TypeMX, dns.TypeTXT, dns.TypeSRV,
		dns.TypeHTTPS, dns.TypeSVCB, dns.TypePTR, dns.TypeCNAME, dns.TypeNS,
		dns.TypeSOA, dns.TypeCAA, dns.TypeTLSA, dns.TypeDS, dns.TypeDNSKEY,
		dns.TypeRRSIG, dns.TypeNSEC, dns.TypeNSEC3, dns.TypeNSEC3PARAM,
		dns.TypeANY, dns.TypeNULL, 65400,
	} {
		message := new(dns.Msg).SetQuestion("Example.COM.", typ)
		got, err := UnpackQuery(packMessage(t, message))
		if err != nil || got.Question[0] != message.Question[0] {
			t.Errorf("QTYPE %d changed or rejected: %v", typ, err)
		}
	}
	for _, name := range []string{
		".", "_sip._tcp.example.", "1.0.0.127.in-addr.arpa.",
		"xn--bcher-kva.example.", `\000.example.`, `a\.b.example.`,
		strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61) + ".",
	} {
		if _, err := UnpackQuery(packMessage(t, new(dns.Msg).SetQuestion(name, dns.TypeA))); err != nil {
			t.Errorf("valid DNS name %q rejected: %v", name, err)
		}
	}
	for _, class := range []uint16{dns.ClassINET, dns.ClassCHAOS, dns.ClassANY, 65400} {
		message := new(dns.Msg).SetQuestion("version.bind.", dns.TypeTXT)
		message.Question[0].Qclass = class
		message.Zero, message.AuthenticatedData, message.CheckingDisabled = true, true, true
		message.SetEdns0(4096, true)
		message.IsEdns0().SetVersion(1)
		message.IsEdns0().Option = []dns.EDNS0{
			&dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET, Family: 1, SourceNetmask: 24, Address: net.IPv4(192, 0, 2, 0)},
			&dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: "0123456789abcdef"},
			&dns.EDNS0_PADDING{Padding: make([]byte, 15)},
			&dns.EDNS0_LOCAL{Code: 65001, Data: []byte{0, 1, 2, 255}},
			&dns.EDNS0_LOCAL{Code: 65002},
		}
		got, err := UnpackQuery(packMessage(t, message))
		if err != nil {
			t.Fatalf("EDNS/flags/class extensions rejected: %v", err)
		}
		if !got.Zero || !got.AuthenticatedData || !got.CheckingDisabled || !got.IsEdns0().Do() || got.IsEdns0().Version() != 1 || len(got.IsEdns0().Option) != 5 {
			t.Fatal("DNS extensions were altered")
		}
	}
}

func TestResponseRecordFormats(t *testing.T) {
	request := new(dns.Msg).SetQuestion("example.com.", dns.TypeANY)
	for _, text := range []string{
		"example.com. 60 IN A 192.0.2.1", "example.com. 60 IN AAAA 2001:db8::1",
		"example.com. 60 IN MX 10 mail.example.com.", `example.com. 60 IN TXT "hello"`,
		"example.com. 60 IN SRV 10 10 443 target.example.com.",
		"example.com. 60 IN HTTPS 1 . alpn=h2,h3 ipv4hint=192.0.2.1",
		"example.com. 60 IN SVCB 1 target.example.com. port=443",
		"example.com. 60 IN PTR target.example.com.",
		"example.com. 60 IN DNSKEY 256 3 8 AQIDBA==",
		"example.com. 60 IN DS 12345 8 2 0123456789ABCDEF",
		"example.com. 60 IN RRSIG A 8 2 60 20270101000000 20250101000000 12345 example.com. AQIDBA==",
		"example.com. 60 IN NSEC next.example.com. A AAAA RRSIG NSEC",
		"example.com. 60 IN NSEC3 1 0 1 AABB 0P9MHAVEQVM6T7VBL5LOP2U3T2RP3TOM A AAAA RRSIG",
		`example.com. 60 IN TYPE65400 \# 4 DEADBEEF`,
		`example.com. 60 IN TYPE65400 \# 0`,
	} {
		rr, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		response := new(dns.Msg).SetReply(request)
		response.Compress = true
		response.Answer = []dns.RR{rr}
		if _, err := Unpack(packMessage(t, response)); err != nil {
			t.Errorf("RR format %d rejected: %v", rr.Header().Rrtype, err)
		}
	}
	// Zero bytes are meaningful for OPT, NULL, APL and unknown RR payloads.
	// Do not reject them as a side effect of checking fixed-width addresses.
	for _, rr := range []dns.RR{
		&dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT, Class: 1232}},
		&dns.NULL{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeNULL, Class: dns.ClassINET}},
		&dns.APL{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeAPL, Class: dns.ClassINET}},
		&dns.RFC3597{Hdr: dns.RR_Header{Name: ".", Rrtype: 65400, Class: dns.ClassINET}},
	} {
		response := new(dns.Msg).SetReply(request)
		response.Extra = []dns.RR{rr}
		if _, err := Unpack(packMessage(t, response)); err != nil {
			t.Errorf("legal empty RR %d rejected: %v", rr.Header().Rrtype, err)
		}
	}
}

func TestMalformedCountsAndMessageBoundaries(t *testing.T) {
	query := packMessage(t, new(dns.Msg).SetQuestion("example.com.", dns.TypeA))
	for _, offset := range []int{4, 6, 8, 10} {
		wire := append([]byte(nil), query...)
		binary.BigEndian.PutUint16(wire[offset:offset+2], binary.BigEndian.Uint16(wire[offset:offset+2])+1)
		if _, err := Unpack(wire); err == nil {
			t.Errorf("missing section at header offset %d accepted", offset)
		}
	}
	edns := new(dns.Msg).SetQuestion("example.com.", dns.TypeAAAA)
	edns.SetEdns0(1232, true)
	missingOPT := packMessage(t, edns)
	binary.BigEndian.PutUint16(missingOPT[10:12], 2)
	for name, wire := range map[string][]byte{
		"empty": nil, "short": make([]byte, 11), "oversize": make([]byte, MaxSize+1),
		"HTTP": []byte("GET / HTTP/1.1\r\n"), "SSH": []byte("SSH-2.0-OpenSSH_9.9\r\n"),
		"trailing data":  append(append([]byte(nil), query...), 0),
		"missing qclass": query[:len(query)-2], "missing qtype and qclass": query[:len(query)-4],
		"missing second OPT": missingOPT,
		"looping name":       append(append([]byte(nil), query[:12]...), 0xc0, 0x0c, 0, 1, 0, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := UnpackQuery(wire); err == nil {
				t.Fatal("malformed query accepted")
			}
		})
	}
	// miekg/dns also tolerates missing sections after a bare twelve-byte header.
	header := make([]byte, 12)
	header[2], header[3], header[5] = 0x80, byte(dns.RcodeRefused), 1
	if _, err := Unpack(header); err == nil {
		t.Fatal("header-only response claiming a question accepted")
	}
}

func TestQueryScopeAndOPTValidation(t *testing.T) {
	query := new(dns.Msg).SetQuestion("example.com.", dns.TypeA)
	for name, mutate := range map[string]func(*dns.Msg){
		"response":        func(m *dns.Msg) { m.Response = true },
		"truncated query": func(m *dns.Msg) { m.Truncated = true },
		"error query":     func(m *dns.Msg) { m.Rcode = dns.RcodeRefused },
		"UPDATE":          func(m *dns.Msg) { m.Opcode = dns.OpcodeUpdate },
		"NOTIFY":          func(m *dns.Msg) { m.Opcode = dns.OpcodeNotify },
		"AXFR":            func(m *dns.Msg) { m.Question[0].Qtype = dns.TypeAXFR },
		"IXFR":            func(m *dns.Msg) { m.Question[0].Qtype = dns.TypeIXFR },
		"no questions":    func(m *dns.Msg) { m.Question = nil },
		"two questions":   func(m *dns.Msg) { m.Question = append(m.Question, m.Question[0]) },
		"answer in query": func(m *dns.Msg) {
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.IPv4(192, 0, 2, 1)}}
		},
		"authority in query": func(m *dns.Msg) {
			m.Ns = []dns.RR{&dns.NS{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeNS, Class: dns.ClassINET}, Ns: "ns.example."}}
		},
		"two OPT records": func(m *dns.Msg) { m.SetEdns0(1232, false); m.Extra = append(m.Extra, dns.Copy(m.Extra[0])) },
		"non-root OPT":    func(m *dns.Msg) { m.SetEdns0(1232, false); m.Extra[0].Header().Name = "example.com." },
	} {
		t.Run(name, func(t *testing.T) {
			message := query.Copy()
			mutate(message)
			if _, err := UnpackQuery(packMessage(t, message)); err == nil {
				t.Fatal("unsupported query accepted")
			}
		})
	}
	for _, typ := range []uint16{dns.TypeA, dns.TypeAAAA} {
		message := query.Copy()
		message.Extra = []dns.RR{&dns.RFC3597{Hdr: dns.RR_Header{Name: ".", Rrtype: typ, Class: dns.ClassINET}}}
		if _, err := UnpackQuery(packMessage(t, message)); err == nil {
			t.Errorf("empty address RR %d accepted", typ)
		}
	}
	for _, authority := range []bool{false, true} {
		message := new(dns.Msg).SetReply(query)
		opt := &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT, Class: 1232}}
		if authority {
			message.Ns = []dns.RR{opt}
		} else {
			message.Answer = []dns.RR{opt}
		}
		if _, err := Unpack(packMessage(t, message)); err == nil {
			t.Fatal("OPT outside additional section accepted")
		}
	}
}

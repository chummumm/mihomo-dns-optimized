// Package dnsmessage validates wire messages for DNS classification and routing.
// It does not resolve names or validate DNSSEC signatures.
package dnsmessage

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/miekg/dns"
)

const MaxSize = 65535

// UnpackQuery accepts one ordinary DNS question. Unknown QTYPEs, classes,
// flags, EDNS versions and options remain available for protocol extensions.
func UnpackQuery(wire []byte) (*dns.Msg, error) {
	message, err := Unpack(wire)
	if err != nil {
		return nil, err
	}
	if message.Response || message.Opcode != dns.OpcodeQuery || message.Rcode != dns.RcodeSuccess || message.Truncated || len(message.Question) != 1 || len(message.Answer) != 0 || len(message.Ns) != 0 {
		return nil, errors.New("DNS proxy accepts only ordinary single-question DNS queries")
	}
	question := message.Question[0]
	if question.Qtype == dns.TypeAXFR || question.Qtype == dns.TypeIXFR {
		return nil, errors.New("DNS proxy does not support zone transfers")
	}
	if _, ok := dns.IsDomainName(question.Name); !ok {
		return nil, errors.New("DNS proxy query has an invalid question name")
	}
	return message, nil
}

// Unpack checks the complete message boundary as well as the section counts.
// dns.Msg.Unpack intentionally tolerates missing records and trailing bytes;
// neither is suitable for identifying messages at a DNS-only proxy boundary.
func Unpack(wire []byte) (*dns.Msg, error) {
	if len(wire) < 12 || len(wire) > MaxSize {
		return nil, errors.New("DNS proxy message length is out of bounds")
	}
	message := new(dns.Msg)
	if err := message.Unpack(wire); err != nil {
		return nil, fmt.Errorf("malformed DNS proxy message: %w", err)
	}
	counts := [...]int{len(message.Question), len(message.Answer), len(message.Ns), len(message.Extra)}
	for index, count := range counts {
		if int(binary.BigEndian.Uint16(wire[4+index*2:6+index*2])) != count {
			return nil, errors.New("DNS proxy section count does not match its contents")
		}
	}
	off := 12
	for range message.Question {
		_, next, err := dns.UnpackDomainName(wire, off)
		if err != nil || next+4 > len(wire) {
			return nil, errors.New("malformed DNS proxy question")
		}
		off = next + 4
	}
	optSeen := false
	for section, records := range [][]dns.RR{message.Answer, message.Ns, message.Extra} {
		for range records {
			rr, next, err := dns.UnpackRR(wire, off)
			if err != nil || next <= off {
				return nil, errors.New("malformed DNS proxy resource record")
			}
			header := rr.Header()
			switch header.Rrtype {
			case dns.TypeOPT:
				if section != 2 || optSeen || header.Name != "." {
					return nil, errors.New("DNS proxy requires one root-owned OPT record in the additional section")
				}
				optSeen = true
			case dns.TypeA:
				if header.Rdlength != 4 {
					return nil, errors.New("DNS proxy A record must contain four address bytes")
				}
			case dns.TypeAAAA:
				if header.Rdlength != 16 {
					return nil, errors.New("DNS proxy AAAA record must contain sixteen address bytes")
				}
			}
			off = next
		}
	}
	if off != len(wire) {
		return nil, errors.New("DNS proxy message contains trailing data")
	}
	return message, nil
}

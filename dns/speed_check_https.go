package dns

import (
	"context"
	"net"
	"net/netip"
	"time"

	D "github.com/miekg/dns"
)

// HTTPS hints are candidates only for their own ServiceMode binding. AliasMode
// delegates resolution to the client; it must not start extra DNS lookups here.
func speedCheckHTTPSHints(record *D.HTTPS) []netip.Addr {
	if record.Priority == 0 || record.Hdr.Class != D.ClassINET {
		return nil
	}
	var hints []netip.Addr
	for _, param := range record.Value {
		var addresses []net.IP
		ipv4 := false
		switch value := param.(type) {
		case *D.SVCBIPv4Hint:
			addresses, ipv4 = value.Hint, true
		case *D.SVCBIPv6Hint:
			addresses = value.Hint
		default:
			continue
		}
		for _, address := range addresses {
			ip, ok := netip.AddrFromSlice(address)
			if ipv4 {
				ip = ip.Unmap()
				ok = ok && ip.Is4()
			} else {
				ok = ok && ip.Is6() && !ip.Is4In6()
			}
			if ok && !ip.IsUnspecified() && !ip.IsMulticast() && ip != netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
				hints = append(hints, ip)
			}
		}
	}
	return hints
}

func speedCheckHTTPSCandidates(message *D.Msg, owner string) []speedCheckCandidate {
	var candidates []speedCheckCandidate
	for _, rr := range message.Answer {
		record, ok := rr.(*D.HTTPS)
		if !ok || !speedCheckTerminalAddress(rr, owner, D.TypeHTTPS) {
			continue
		}
		for _, ip := range speedCheckHTTPSHints(record) {
			candidates = append(candidates, speedCheckCandidate{ip: ip, owner: owner, response: message})
		}
	}
	return candidates
}

// Keep one intact upstream answer and select the fastest measured hint within
// each service binding, using SmartDNS's fastest-hint behavior. Hints never cross
// record/upstream boundaries: target, priority, ECH, ALPN, port and unknown
// parameters still describe exactly the endpoint that supplied the address.
// A binding with no successful measurement retains all its original hints.
func speedCheckHTTPSAnswer(ctx context.Context, original *D.Msg, question D.Question, measurements map[netip.Addr]time.Duration) *D.Msg {
	answer := original.Copy()
	inheritDNSAnswerLifetime(ctx, answer, original)
	chain, valid := speedCheckResponseChain(original, question)
	if !valid || speedCheckProtected(original) {
		return answer
	}
	for _, rr := range answer.Answer {
		record, ok := rr.(*D.HTTPS)
		if !ok || !speedCheckTerminalAddress(rr, chain.owner, D.TypeHTTPS) {
			continue
		}
		var best netip.Addr
		var fastest time.Duration
		for _, ip := range speedCheckHTTPSHints(record) {
			if rtt, measured := measurements[ip]; measured && rtt >= 0 &&
				(!best.IsValid() || rtt < fastest || (rtt == fastest && ip.Less(best))) {
				best, fastest = ip, rtt
			}
		}
		if best.IsValid() {
			speedCheckSelectHTTPSHint(record, best)
		}
	}
	return answer
}

// Removing one hint family must also remove its mandatory reference. Every
// other parameter (including unknown extensions) is preserved in place.
func speedCheckSelectHTTPSHint(record *D.HTTPS, ip netip.Addr) {
	values := make([]D.SVCBKeyValue, 0, len(record.Value))
	for _, param := range record.Value {
		switch value := param.(type) {
		case *D.SVCBIPv4Hint:
			if ip.Is4() {
				values = append(values, &D.SVCBIPv4Hint{Hint: []net.IP{ip.AsSlice()}})
			}
		case *D.SVCBIPv6Hint:
			if ip.Is6() {
				values = append(values, &D.SVCBIPv6Hint{Hint: []net.IP{ip.AsSlice()}})
			}
		case *D.SVCBMandatory:
			var keys []D.SVCBKey
			for _, key := range value.Code {
				if (key == D.SVCB_IPV4HINT && ip.Is6()) || (key == D.SVCB_IPV6HINT && ip.Is4()) {
					continue
				}
				keys = append(keys, key)
			}
			if len(keys) != 0 {
				values = append(values, &D.SVCBMandatory{Code: keys})
			}
		default:
			values = append(values, param)
		}
	}
	record.Value = values
}

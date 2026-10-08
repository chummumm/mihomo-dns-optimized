package tunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
	"github.com/metacubex/mihomo/log"

	"github.com/miekg/dns"
)

const (
	dnsProxyTimeout    = 5 * time.Second
	dnsProxyMaxDepth   = 64
	dnsProxyMaxMessage = 65535
)

var errDNSProxyDrop = errors.New("DNS proxy query rejected without a response")

type dnsProxyRoute struct {
	proxy C.Proxy
	rule  C.Rule
	// Groups are chosen using QNAME and frozen for this one exchange. Dialing
	// the group with resolver metadata would hash the DNS server's IP instead.
	groups []C.Proxy
}

type dnsProxySelect func(*C.Metadata) (dnsProxyRoute, error)
type dnsProxyExchange func(context.Context, []byte, *C.Metadata, dnsProxyRoute) ([]byte, error)

// ExchangeDNS routes one DNS question using the domain-evaluable portion of
// the live traffic rules, then sends the original wire message to its original
// resolver. It never resolves QNAME to evaluate IP or process rules, and never
// uses the resolver's IP as the requested site's destination IP.
//
// Call once per UDP datagram or DNS-over-TCP message, not once per connection.
// A literal resolver IP is required to avoid circular DNS bootstrap.
func (t tunnel) ExchangeDNS(ctx context.Context, query []byte, resolver *C.Metadata) ([]byte, error) {
	return exchangeDNSProxy(ctx, query, resolver, selectDNSProxy, exchangeDNSProxyWire)
}

var _ C.DNSExchanger = Tunnel

func exchangeDNSProxy(ctx context.Context, wire []byte, resolver *C.Metadata, selectProxy dnsProxySelect, exchange dnsProxyExchange) ([]byte, error) {
	if resolver == nil || resolver.DstPort != 53 || resolver.Host != "" || !(resolver.DstIP.IsGlobalUnicast() || resolver.DstIP.IsLoopback() || resolver.DstIP.IsLinkLocalUnicast()) {
		return nil, errors.New("DNS proxy requires a literal unicast resolver IP on destination port 53")
	}
	if resolver.NetWork != C.TCP && resolver.NetWork != C.UDP {
		return nil, errors.New("DNS proxy requires TCP or UDP")
	}
	request, err := unpackDNSProxyMessage(wire)
	if err != nil {
		return nil, err
	}
	if request.Response || request.Opcode != dns.OpcodeQuery || request.Rcode != dns.RcodeSuccess || request.Truncated || len(request.Question) != 1 || len(request.Answer) != 0 || len(request.Ns) != 0 {
		return nil, errors.New("DNS proxy accepts only ordinary single-question DNS queries")
	}
	question := request.Question[0]
	if question.Qtype == dns.TypeAXFR || question.Qtype == dns.TypeIXFR {
		return nil, errors.New("DNS proxy does not support zone transfers")
	}
	if _, ok := dns.IsDomainName(question.Name); !ok {
		return nil, errors.New("DNS proxy query has an invalid question name")
	}
	host := strings.ToLower(strings.TrimSuffix(question.Name, "."))
	// The root name is a valid DNS question and should reach MATCH.
	if host == "" {
		host = "."
	}
	ctx, cancel := context.WithTimeout(ctx, dnsProxyTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Only the question's name is business metadata. SmartDNS's source address,
	// process, port and protocol describe the resolver, not its original client.
	metadata := &C.Metadata{Host: host, Type: C.INNER, NetWork: resolver.NetWork}
	route, err := selectProxy(metadata)
	if err != nil {
		return nil, err
	}
	if route.proxy == nil {
		return nil, errors.New("DNS proxy selected an unavailable outbound")
	}
	switch route.proxy.Type() {
	case C.Reject:
		reply := new(dns.Msg).SetRcode(request, dns.RcodeRefused)
		return reply.Pack()
	case C.RejectDrop:
		return nil, errDNSProxyDrop
	case C.Dns, C.Rematch, C.Pass, C.PassRule:
		return nil, fmt.Errorf("DNS proxy cannot exchange through outbound type %s", route.proxy.Type())
	}
	if resolver.NetWork == C.UDP && !route.proxy.SupportUDP() {
		// Never fall through to another rule or DIRECT just because the chosen
		// outbound cannot carry UDP. SmartDNS can use TCP upstreams in that case.
		return nil, fmt.Errorf("DNS proxy outbound %q does not support UDP", route.proxy.Name())
	}
	destination := resolver.Clone()
	destination.DstIP = destination.DstIP.Unmap()
	destination.Host = ""
	destination.SniffHost = ""
	destination.DNSMode = C.DNSNormal
	destination.SpecialProxy = ""
	destination.SpecialRules = ""
	response, err := exchange(ctx, wire, destination, route)
	if err != nil {
		return nil, err
	}
	if err := validateDNSProxyResponse(request, response); err != nil {
		return nil, err
	}
	chain := route.proxy.Name()
	for index := len(route.groups) - 1; index >= 0; index-- {
		chain = route.groups[index].Name() + "[" + chain + "]"
	}
	if route.rule != nil {
		log.Debugln("[DNS proxy] %s (%s) --> %s match %s(%s) using %s", host, dns.TypeToString[question.Qtype], destination.RemoteAddress(), route.rule.RuleType(), route.rule.Payload(), chain)
	} else {
		log.Debugln("[DNS proxy] %s (%s) --> %s using %s", host, dns.TypeToString[question.Qtype], destination.RemoteAddress(), chain)
	}
	return response, nil
}

// Unpack additionally rejects trailing bytes; dns.Msg.Unpack deliberately
// accepts them, which is unsuitable for a DNS-only proxy boundary.
func unpackDNSProxyMessage(wire []byte) (*dns.Msg, error) {
	if len(wire) < 12 || len(wire) > dnsProxyMaxMessage {
		return nil, errors.New("DNS proxy message length is out of bounds")
	}
	message := new(dns.Msg)
	if err := message.Unpack(wire); err != nil {
		return nil, fmt.Errorf("malformed DNS proxy message: %w", err)
	}
	off := 12
	for range message.Question {
		_, next, err := dns.UnpackDomainName(wire, off)
		if err != nil || next+4 > len(wire) {
			return nil, errors.New("malformed DNS proxy question")
		}
		off = next + 4
	}
	for count := len(message.Answer) + len(message.Ns) + len(message.Extra); count > 0; count-- {
		_, next, err := dns.UnpackRR(wire, off)
		if err != nil || next <= off {
			return nil, errors.New("malformed DNS proxy resource record")
		}
		off = next
	}
	if off != len(wire) {
		return nil, errors.New("DNS proxy message contains trailing data")
	}
	return message, nil
}

func validateDNSProxyResponse(request *dns.Msg, wire []byte) error {
	response, err := unpackDNSProxyMessage(wire)
	if err != nil {
		return err
	}
	if !response.Response || response.Opcode != request.Opcode || response.Id != request.Id || len(response.Question) != 1 {
		return errors.New("DNS proxy received a mismatched response")
	}
	question, answerQuestion := request.Question[0], response.Question[0]
	if !strings.EqualFold(question.Name, answerQuestion.Name) || question.Qtype != answerQuestion.Qtype || question.Qclass != answerQuestion.Qclass {
		return errors.New("DNS proxy received a response for a different question")
	}
	return nil
}

func exchangeDNSProxyWire(ctx context.Context, query []byte, resolver *C.Metadata, route dnsProxyRoute) ([]byte, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("DNS proxy exchange requires a deadline")
	}
	if resolver.NetWork == C.TCP {
		conn, err := route.proxy.DialContext(ctx, resolver)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
		stop := closeDNSProxyOnCancel(ctx, conn)
		defer stop()
		frame := make([]byte, len(query)+2)
		binary.BigEndian.PutUint16(frame, uint16(len(query)))
		copy(frame[2:], query)
		if err := writeDNSProxyFrame(conn, frame); err != nil {
			return nil, err
		}
		var length [2]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return nil, err
		}
		n := int(binary.BigEndian.Uint16(length[:]))
		if n < 12 {
			return nil, errors.New("DNS proxy received an invalid TCP frame length")
		}
		response := make([]byte, n)
		if _, err := io.ReadFull(conn, response); err != nil {
			return nil, err
		}
		return response, nil
	}
	conn, err := route.proxy.ListenPacketContext(ctx, resolver)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	stop := closeDNSProxyOnCancel(ctx, conn)
	defer stop()
	if n, err := conn.WriteTo(query, resolver.UDPAddr()); err != nil {
		return nil, err
	} else if n != len(query) {
		return nil, io.ErrShortWrite
	}
	response := make([]byte, dnsProxyMaxMessage)
	for {
		n, from, err := conn.ReadFrom(response)
		if err != nil {
			return nil, err
		}
		if !sameDNSProxyEndpoint(from, resolver.AddrPort()) {
			// Ignore unrelated packets until the bounded exchange deadline.
			continue
		}
		return response[:n], nil
	}
}

func writeDNSProxyFrame(w io.Writer, frame []byte) error {
	for len(frame) > 0 {
		n, err := w.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}

// Also interrupt blocked reads promptly when the caller cancels (for example
// when an inbound is closed), rather than waiting for the complete per-query
// deadline. Uses Go 1.20-compatible APIs.
func closeDNSProxyOnCancel(ctx context.Context, conn io.Closer) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func sameDNSProxyEndpoint(addr net.Addr, expected netip.AddrPort) bool {
	if addr == nil {
		return false
	}
	metadata := new(C.Metadata)
	if err := metadata.SetRemoteAddr(addr); err != nil || !metadata.DstIP.IsValid() {
		return false
	}
	return metadata.AddrPort() == expected
}

func selectDNSProxy(metadata *C.Metadata) (dnsProxyRoute, error) {
	configMux.RLock()
	defer configMux.RUnlock()
	if mode == Direct {
		return unwrapDNSProxy(proxies["DIRECT"], nil, metadata)
	}
	if mode == Global {
		return unwrapDNSProxy(proxies["GLOBAL"], nil, metadata)
	}
	for _, rule := range rules {
		result := matchDNSProxyRule(rule, metadata, 0)
		if !result.known || !result.match {
			continue
		}
		proxy, ok := proxies[result.adapter]
		if !ok {
			continue // Same behavior as normal routing for a missing adapter.
		}
		route, err := unwrapDNSProxy(proxy, rule, metadata)
		if err != nil {
			return route, err
		}
		if route.proxy.Type() == C.Pass {
			continue
		}
		return route, nil
	}
	return unwrapDNSProxy(proxies["DIRECT"], nil, metadata)
}

func unwrapDNSProxy(proxy C.Proxy, rule C.Rule, metadata *C.Metadata) (dnsProxyRoute, error) {
	return unwrapDNSProxyWithTouch(proxy, rule, metadata, true)
}

func unwrapDNSProxyWithTouch(proxy C.Proxy, rule C.Rule, metadata *C.Metadata, touch bool) (dnsProxyRoute, error) {
	route := dnsProxyRoute{proxy: proxy, rule: rule}
	for depth := 0; depth < dnsProxyMaxDepth; depth++ {
		if route.proxy == nil {
			return route, errors.New("DNS proxy outbound is unavailable")
		}
		if metadata.NetWork == C.UDP && !route.proxy.SupportUDP() {
			return route, fmt.Errorf("DNS proxy outbound %q does not support UDP", route.proxy.Name())
		}
		next := route.proxy.Unwrap(metadata, touch)
		if next == nil {
			return route, nil
		}
		route.groups = append(route.groups, route.proxy)
		route.proxy = next
	}
	return route, errors.New("DNS proxy outbound group nesting is too deep or cyclic")
}

// Unknown is not false: NOT(IP-CIDR,...) must not become true just because DNS
// routing deliberately did not resolve the destination IP. The same rule
// applies to mixed classical providers and nested logical conditions.
type dnsProxyMatch struct {
	match   bool
	known   bool
	adapter string
}

type dnsProxyRuleChildren interface {
	Rules() []C.Rule
}

func matchDNSProxyRule(rule C.Rule, metadata *C.Metadata, depth int) dnsProxyMatch {
	if depth >= dnsProxyMaxDepth {
		return dnsProxyMatch{}
	}
	if wrapped, ok := rule.(C.RuleWrapper); ok {
		if wrapped.IsDisabled() {
			return dnsProxyMatch{known: true}
		}
		result := matchDNSProxyRule(wrapped.Unwrap(), metadata, depth+1)
		if count, ok := wrapped.(interface {
			Hit()
			Miss()
		}); ok && result.known {
			if result.match {
				count.Hit()
			} else {
				count.Miss()
			}
		}
		return result
	}
	switch rule.RuleType() {
	case C.Domain, C.DomainSuffix, C.DomainKeyword, C.DomainRegex, C.DomainWildcard, C.GEOSITE, C.MATCH:
		matched, adapter := rule.Match(metadata, C.RuleMatchHelper{})
		return dnsProxyMatch{match: matched, known: true, adapter: adapter}
	case C.RuleSet:
		provider, ok := ruleProviders[rule.Payload()]
		if !ok {
			return dnsProxyMatch{}
		}
		switch provider.Behavior() {
		case P.Domain:
			return dnsProxyMatch{match: provider.Match(metadata, C.RuleMatchHelper{}), known: true, adapter: rule.Adapter()}
		case P.Classical:
			if children, ok := provider.Strategy().(dnsProxyRuleChildren); ok {
				result := matchDNSProxyChildren(children.Rules(), C.OR, metadata, depth+1)
				result.adapter = rule.Adapter()
				return result
			}
		}
	case C.AND, C.OR, C.NOT:
		if children, ok := rule.(dnsProxyRuleChildren); ok {
			result := matchDNSProxyChildren(children.Rules(), rule.RuleType(), metadata, depth+1)
			result.adapter = rule.Adapter()
			return result
		}
	case C.SubRules:
		if children, ok := rule.(dnsProxyRuleChildren); ok && len(children.Rules()) == 1 {
			condition := matchDNSProxyRule(children.Rules()[0], metadata, depth+1)
			if !condition.known || !condition.match {
				return condition
			}
			for _, child := range subRules[rule.Adapter()] {
				result := matchDNSProxyRule(child, metadata, depth+1)
				if !result.known || !result.match {
					continue
				}
				if proxy, ok := proxies[result.adapter]; ok {
					route, err := unwrapDNSProxyWithTouch(proxy, child, metadata, false)
					if err == nil && route.proxy.Type() == C.PassRule {
						continue
					}
				}
				return result
			}
			return dnsProxyMatch{known: true}
		}
	}
	return dnsProxyMatch{}
}

func matchDNSProxyChildren(children []C.Rule, ruleType C.RuleType, metadata *C.Metadata, depth int) dnsProxyMatch {
	if ruleType == C.NOT {
		if len(children) != 1 {
			return dnsProxyMatch{}
		}
		result := matchDNSProxyRule(children[0], metadata, depth)
		if result.known {
			result.match = !result.match
		}
		return result
	}
	unknown := false
	for _, child := range children {
		result := matchDNSProxyRule(child, metadata, depth)
		if !result.known {
			unknown = true
			continue
		}
		if ruleType == C.OR && result.match {
			return dnsProxyMatch{match: true, known: true}
		}
		if ruleType == C.AND && !result.match {
			return dnsProxyMatch{known: true}
		}
	}
	return dnsProxyMatch{match: ruleType == C.AND && !unknown, known: !unknown}
}

package dns

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/picker"
	"github.com/metacubex/mihomo/component/ech/echparser"
	"github.com/metacubex/mihomo/component/resolver"
	"github.com/metacubex/mihomo/log"

	D "github.com/miekg/dns"
	"github.com/samber/lo"
	"golang.org/x/exp/slices"
)

const (
	MaxMsgSize = 65535
)

const serverFailureCacheTTL uint32 = 5

func minimalTTL(records []D.RR) uint32 {
	var ttl uint32
	found := false
	for _, rr := range records {
		if rr.Header().Rrtype == D.TypeOPT {
			continue // OPT carries EDNS control bits, never a record lifetime.
		}
		if !found || rr.Header().Ttl < ttl {
			ttl, found = rr.Header().Ttl, true
		}
	}
	return ttl
}

func updateTTL(records []D.RR, ttl uint32) {
	if len(records) == 0 {
		return
	}
	minimum := minimalTTL(records)
	var delta uint32
	if minimum > ttl {
		delta = minimum - ttl
	}
	for _, rr := range records {
		header := rr.Header()
		if header.Rrtype == D.TypeOPT || header.Ttl == 0 {
			continue
		}
		// Keep the existing positive-TTL cache-hit floor without unsigned
		// underflow, changing EDNS flags, or making a zero-TTL RR cacheable.
		if delta >= header.Ttl {
			header.Ttl = 1
		} else {
			header.Ttl -= delta
		}
	}
}

type dnsAnswerLifetimesKey struct{}

// A lifetime table belongs to one actual resolver exchange, including its
// main/fallback and dual-stack work. It is never stored with a cached reply or
// shared between unrelated resolver invocations.
type dnsAnswerLifetimes struct {
	mu       sync.Mutex
	received map[*D.Msg]time.Time
	expires  map[*D.Msg]time.Time
}

func withDNSAnswerLifetimes(ctx context.Context) context.Context {
	return context.WithValue(ctx, dnsAnswerLifetimesKey{}, &dnsAnswerLifetimes{
		received: make(map[*D.Msg]time.Time),
	})
}

func hasDNSAnswerLifetimes(ctx context.Context) bool {
	lifetimes, _ := ctx.Value(dnsAnswerLifetimesKey{}).(*dnsAnswerLifetimes)
	return lifetimes != nil
}

func recordDNSAnswerReceived(ctx context.Context, message *D.Msg, receivedAt time.Time) time.Time {
	lifetimes, _ := ctx.Value(dnsAnswerLifetimesKey{}).(*dnsAnswerLifetimes)
	if lifetimes == nil || message == nil || receivedAt.IsZero() {
		return receivedAt
	}
	lifetimes.mu.Lock()
	defer lifetimes.mu.Unlock()
	// A raw candidate hit already knows when its upstream response arrived.
	// A wrapper or collector must not reset that clock on the next handoff.
	if previous, ok := lifetimes.received[message]; !ok || receivedAt.Before(previous) {
		lifetimes.received[message] = receivedAt
	}
	return lifetimes.received[message]
}

func inheritDNSAnswerLifetime(ctx context.Context, message, original *D.Msg) {
	lifetimes, _ := ctx.Value(dnsAnswerLifetimesKey{}).(*dnsAnswerLifetimes)
	if lifetimes == nil || message == nil || original == nil || message == original {
		return
	}
	lifetimes.mu.Lock()
	defer lifetimes.mu.Unlock()
	if receivedAt, ok := lifetimes.received[original]; ok {
		if previous, ok := lifetimes.received[message]; !ok || receivedAt.Before(previous) {
			lifetimes.received[message] = receivedAt
		}
	}
	if expires, ok := lifetimes.expires[original]; ok {
		if previous, ok := lifetimes.expires[message]; !ok || expires.Before(previous) {
			lifetimes.expires[message] = expires
		}
	}
}

// TTL policy applies to the lifetime received from upstream, before subtracting
// the time spent collecting and probing. Applying a minimum after this aging
// would grant an already-expired answer a new cache lifetime.
func ageDNSAnswerAfterPolicy(ctx context.Context, original, adjusted *D.Msg) *D.Msg {
	lifetimes, _ := ctx.Value(dnsAnswerLifetimesKey{}).(*dnsAnswerLifetimes)
	if lifetimes == nil || original == nil || adjusted == nil {
		return adjusted
	}
	lifetimes.mu.Lock()
	receivedAt, ok := lifetimes.received[original]
	lifetimes.mu.Unlock()
	if !ok {
		return adjusted
	}
	answer := ageDNSAnswerAt(adjusted, receivedAt, time.Now())
	// DNS wire TTLs have whole-second precision, but cache expiration must
	// retain the fractional second already spent probing. Otherwise rounding
	// the elapsed time down and storing at now+TTL silently extends validity.
	expires := receivedAt.Add(time.Duration(dnsMessageCacheTTL(adjusted)) * time.Second)
	lifetimes.mu.Lock()
	defer lifetimes.mu.Unlock()
	if previous, ok := lifetimes.expires[original]; ok && previous.Before(expires) {
		expires = previous
	}
	if previous, ok := lifetimes.expires[answer]; ok && previous.Before(expires) {
		expires = previous
	}
	if lifetimes.expires == nil {
		lifetimes.expires = make(map[*D.Msg]time.Time)
	}
	lifetimes.expires[answer] = expires
	return answer
}

func dnsAnswerExpires(ctx context.Context, message *D.Msg) time.Time {
	lifetimes, _ := ctx.Value(dnsAnswerLifetimesKey{}).(*dnsAnswerLifetimes)
	if lifetimes == nil || message == nil {
		return time.Time{}
	}
	lifetimes.mu.Lock()
	defer lifetimes.mu.Unlock()
	return lifetimes.expires[message]
}

func ageDNSAnswerAt(message *D.Msg, receivedAt, now time.Time) *D.Msg {
	if message == nil || receivedAt.IsZero() || !now.After(receivedAt) {
		return message
	}
	elapsed := uint64(now.Sub(receivedAt) / time.Second)
	if elapsed == 0 {
		return message
	}
	for _, records := range [][]D.RR{message.Answer, message.Ns, message.Extra} {
		for _, rr := range records {
			// TSIG and SIG(0) authenticate the whole transaction, including
			// ordinary record TTLs. RRSIG instead permits ordinary TTL aging
			// while its signed OrigTtl and signature fields remain unchanged.
			if rr.Header().Rrtype == D.TypeTSIG {
				return message
			}
			if signature, ok := rr.(*D.SIG); ok && signature.TypeCovered == 0 {
				return message
			}
		}
	}
	answer := message.Copy()
	for _, records := range [][]D.RR{answer.Answer, answer.Ns, answer.Extra} {
		for _, rr := range records {
			header := rr.Header()
			if header.Rrtype == D.TypeOPT {
				continue
			}
			if elapsed >= uint64(header.Ttl) {
				header.Ttl = 0
			} else {
				header.Ttl -= uint32(elapsed)
			}
		}
	}
	return answer
}

// getMsgFromCache returns a cached dns message if it exists, otherwise returns nil.
// the returned msg is a copy of the original msg, so it can be modified without affecting the original msg.
func getMsgFromCache(c dnsCache, key string) (*D.Msg, time.Time, bool) {
	msg, expireTime, hit := c.GetWithExpire(key)
	if msg != nil {
		msg = msg.Copy() // never modify the original msg
	}
	return msg, expireTime, hit
}

// putMsgToCache puts a dns message into the cache.
// the msg is copied before being stored in the cache, so it can be modified without affecting the original msg.
func putMsgToCache(c dnsCache, key string, q D.Question, msg *D.Msg) {
	putMsgToCacheWithExpiry(c, key, q, msg, time.Time{})
}

func dnsMessageCacheTTL(msg *D.Msg) uint32 {
	if msg.Rcode == D.RcodeServerFailure {
		// [...] a resolver MAY cache a server failure response.
		// If it does so it MUST NOT cache it for longer than five (5) minutes [...]
		return serverFailureCacheTTL
	}
	return minimalTTL(lo.Concat(msg.Answer, msg.Ns, msg.Extra))
}

func putMsgToCacheWithExpiry(c dnsCache, key string, q D.Question, msg *D.Msg, expires time.Time) {
	// skip dns cache for acme challenge
	if q.Qtype == D.TypeTXT && strings.HasPrefix(q.Name, "_acme-challenge.") {
		log.Debugln("[DNS] dns cache ignored because of acme challenge for: %s", q.Name)
		return
	}

	msg = msg.Copy() // never modify the original msg

	// OPT RRs MUST NOT be cached, forwarded, or stored in or loaded from master files.
	msg.Extra = lo.Filter(msg.Extra, func(rr D.RR, index int) bool {
		return rr.Header().Rrtype != D.TypeOPT
	})

	ttl := dnsMessageCacheTTL(msg)
	now := time.Now()
	if ttl == 0 || (!expires.IsZero() && !expires.After(now)) {
		if msg.Rcode == D.RcodeSuccess || msg.Rcode == D.RcodeNameError {
			// A successful fresh answer can deliberately be uncacheable (for
			// example a transient dual-stack preference). It supersedes any
			// older answer, which must not keep returning through serve-expired.
			c.Delete(key)
		}
		return
	}

	remainingExpires := now.Add(time.Duration(ttl) * time.Second)
	if expires.IsZero() || remainingExpires.Before(expires) {
		expires = remainingExpires
	}
	c.SetWithExpire(key, msg, expires)
}

func setMsgTTL(msg *D.Msg, ttl uint32) {
	for _, records := range [][]D.RR{msg.Answer, msg.Ns, msg.Extra} {
		for _, rr := range records {
			if rr.Header().Rrtype == D.TypeOPT { // RFC 6891: EDNS control bits are not a TTL.
				continue
			}
			rr.Header().Ttl = ttl
		}
	}
}

func updateMsgTTL(msg *D.Msg, ttl uint32) {
	updateTTL(msg.Answer, ttl)
	updateTTL(msg.Ns, ttl)
	updateTTL(msg.Extra, ttl)
}

func isIPRequest(q D.Question) bool {
	return q.Qclass == D.ClassINET && (q.Qtype == D.TypeA || q.Qtype == D.TypeAAAA || q.Qtype == D.TypeCNAME)
}

func transform(servers []NameServer, resolver resolver.Resolver) []dnsClient {
	ret := make([]dnsClient, 0, len(servers))
	for _, s := range servers {
		var c dnsClient
		switch s.Net {
		case "tls":
			c = newDoTClient(s.Addr, resolver, s.Params, s.ProxyAdapter, s.ProxyName)
		case "https":
			c = newDoHClient(s.Addr, resolver, s.PreferH3, s.Params, s.ProxyAdapter, s.ProxyName)
		case "dhcp":
			c = newDHCPClient(s.Addr)
		case "system":
			c = newSystemClient()
		case "tailscale":
			c = newTailscaleClient(s.Addr)
		case "easytier":
			c = newEasyTierClient(s.Addr)
		case "rcode":
			c = newRCodeClient(s.Addr)
		case "quic":
			c = newDoQ(s.Addr, resolver, s.Params, s.ProxyAdapter, s.ProxyName)
		default:
			c = newClient(s.Addr, resolver, s.Net, s.Params, s.ProxyAdapter, s.ProxyName)
		}
		c = rewrapClient(c, s.Params)
		ret = append(ret, c)
	}
	return ret
}

// rewrapClient peels any existing wrapper layers off c to reach the raw transport
// client, then re-wraps it according to params. Passing a raw client is fine (the
// peel is a no-op), which lets a shared raw transport be re-wrapped per name server.
func rewrapClient(c dnsClient, params map[string]string) dnsClient {
	for {
		u, ok := c.(interface{ Unwrap() dnsClient })
		if !ok {
			break
		}
		c = u.Unwrap()
	}
	c = wrapClientWithEdns0Subnet(c, params)
	c = wrapClientWithDisableTypes(c, params)
	return c
}

// isWrapperOnlyParam reports whether a param only affects the wrapper layer and not
// the transport connection, so it can be ignored when comparing transports.
func isWrapperOnlyParam(key string) bool {
	return isDisableTypesParam(key) || isEdns0SubnetParam(key)
}

type clientWithDisableTypes struct {
	dnsClient
	disableTypes map[uint16]struct{}
}

func (c clientWithDisableTypes) ExchangeContext(ctx context.Context, m *D.Msg) (msg *D.Msg, err error) {
	// filter dns request
	if slices.ContainsFunc(m.Question, c.inQuestion) {
		// In fact, DNS requests are not allowed to contain multiple questions:
		// https://stackoverflow.com/questions/4082081/requesting-a-and-aaaa-records-in-single-dns-query/4083071
		// so, when we find a question containing the type, we can simply discard the entire dns request.
		msg = handleMsgWithEmptyAnswer(m)
		markDNSLocalAnswer(ctx, msg)
		return msg, nil
	}

	// do real exchange
	msg, err = c.dnsClient.ExchangeContext(ctx, m)
	if err != nil {
		return
	}

	// filter dns response
	msg.Answer = slices.DeleteFunc(msg.Answer, c.inRR)
	msg.Ns = slices.DeleteFunc(msg.Ns, c.inRR)
	msg.Extra = slices.DeleteFunc(msg.Extra, c.inRR)
	return
}

func (c clientWithDisableTypes) inQuestion(q D.Question) bool {
	_, ok := c.disableTypes[q.Qtype]
	return ok
}

func (c clientWithDisableTypes) inRR(rr D.RR) bool {
	_, ok := c.disableTypes[rr.Header().Rrtype]
	return ok
}

func (c clientWithDisableTypes) Unwrap() dnsClient { return c.dnsClient }

// isDisableTypesParam reports the params consumed by wrapClientWithDisableTypes.
func isDisableTypesParam(key string) bool {
	switch key {
	case "disable-ipv4", "disable-ipv6":
		return true
	}
	return strings.HasPrefix(key, "disable-qtype-")
}

func wrapClientWithDisableTypes(c dnsClient, params map[string]string) dnsClient {
	disableTypes := make(map[uint16]struct{})
	if params["disable-ipv4"] == "true" {
		disableTypes[D.TypeA] = struct{}{}
	}
	if params["disable-ipv6"] == "true" {
		disableTypes[D.TypeAAAA] = struct{}{}
	}
	for key, value := range params {
		const prefix = "disable-qtype-"
		if strings.HasPrefix(key, prefix) && value == "true" { // eg: disable-qtype-65=true
			qType, err := strconv.ParseUint(key[len(prefix):], 10, 16)
			if err != nil {
				continue
			}
			if _, ok := D.TypeToRR[uint16(qType)]; !ok { // check valid RR_Header.Rrtype and Question.qtype
				continue
			}
			disableTypes[uint16(qType)] = struct{}{}
		}
	}
	if len(disableTypes) > 0 {
		return clientWithDisableTypes{c, disableTypes}
	}
	return c
}

type clientWithEdns0Subnet struct {
	dnsClient
	ecsPrefix   netip.Prefix
	ecsOverride bool
}

func (c clientWithEdns0Subnet) ExchangeContext(ctx context.Context, m *D.Msg) (*D.Msg, error) {
	m = m.Copy()
	setEdns0Subnet(m, c.ecsPrefix, c.ecsOverride)
	return c.dnsClient.ExchangeContext(ctx, m)
}

func (c clientWithEdns0Subnet) Unwrap() dnsClient { return c.dnsClient }

// isEdns0SubnetParam reports the params consumed by wrapClientWithEdns0Subnet.
func isEdns0SubnetParam(key string) bool {
	switch key {
	case "ecs", "ecs-override":
		return true
	}
	return false
}

func wrapClientWithEdns0Subnet(c dnsClient, params map[string]string) dnsClient {
	var ecsPrefix netip.Prefix
	var ecsOverride bool
	if ecs := params["ecs"]; ecs != "" {
		prefix, err := netip.ParsePrefix(ecs)
		if err != nil {
			addr, err := netip.ParseAddr(ecs)
			if err != nil {
				log.Warnln("DNS [%s] config with invalid ecs: %s", c.Address(), ecs)
			} else {
				ecsPrefix = netip.PrefixFrom(addr, addr.BitLen())
			}
		} else {
			ecsPrefix = prefix
		}
	}

	if ecsPrefix.IsValid() {
		log.Debugln("DNS [%s] config with ecs: %s", c.Address(), ecsPrefix)
		if params["ecs-override"] == "true" {
			ecsOverride = true
		}
		return clientWithEdns0Subnet{c, ecsPrefix, ecsOverride}
	}
	return c
}

func handleMsgWithEmptyAnswer(r *D.Msg) *D.Msg {
	msg := &D.Msg{}
	msg.Answer = []D.RR{}

	msg.SetRcode(r, D.RcodeSuccess)
	msg.Authoritative = true
	msg.RecursionAvailable = true

	return msg
}

func msgToIP(msg *D.Msg) (ips []netip.Addr) {
	for _, answer := range msg.Answer {
		var ip netip.Addr
		switch ans := answer.(type) {
		case *D.AAAA:
			ip, _ = netip.AddrFromSlice(ans.AAAA)
		case *D.A:
			ip, _ = netip.AddrFromSlice(ans.A)
		default:
			continue
		}
		if !ip.IsValid() {
			continue
		}
		ip = ip.Unmap()
		ips = append(ips, ip)
	}
	return
}

func msgToDomain(msg *D.Msg) string {
	if len(msg.Question) > 0 {
		return strings.TrimRight(msg.Question[0].Name, ".")
	}

	return ""
}

func msgToQtype(msg *D.Msg) (uint16, string) {
	if len(msg.Question) > 0 {
		qType := msg.Question[0].Qtype
		return qType, D.Type(qType).String()
	}
	return 0, ""
}

func msgToHTTPSRRInfo(msg *D.Msg) string {
	var alpns []string
	var publicName string
	var hasIPv4, hasIPv6 bool

	collect := func(rrs []D.RR) {
		for _, rr := range rrs {
			httpsRR, ok := rr.(*D.HTTPS)
			if !ok {
				continue
			}

			for _, kv := range httpsRR.Value {
				switch v := kv.(type) {
				case *D.SVCBAlpn:
					if len(alpns) == 0 && len(v.Alpn) > 0 {
						alpns = append(alpns, v.Alpn...)
					}
				case *D.SVCBIPv4Hint:
					if len(v.Hint) > 0 {
						hasIPv4 = true
					}
				case *D.SVCBIPv6Hint:
					if len(v.Hint) > 0 {
						hasIPv6 = true
					}
				case *D.SVCBECHConfig:
					if publicName == "" && len(v.ECH) > 0 {
						if cfgs, err := echparser.ParseECHConfigList(v.ECH); err == nil && len(cfgs) > 0 {
							publicName = string(cfgs[0].PublicName)
						}
					}
				}
			}
		}
	}

	collect(msg.Answer)

	//TODO: Do we need to process the data in msg.Extra?
	//      If so, do we need to validate whether the domain names within it match our request?
	//      To simplify the problem, let's ignore it for now.
	//collect(msg.Extra)

	if len(alpns) == 0 && publicName == "" && !hasIPv4 && !hasIPv6 {
		return ""
	}

	var parts []string
	if len(alpns) > 0 {
		parts = append(parts, "alpn:"+strings.Join(alpns, ","))
	}
	if publicName != "" {
		parts = append(parts, "pn:"+publicName)
	}
	if hasIPv4 {
		parts = append(parts, "ipv4hint")
	}
	if hasIPv6 {
		parts = append(parts, "ipv6hint")
	}

	return strings.Join(parts, ";")
}

func msgToLogString(msg *D.Msg) string {
	qType, qTypeStr := msgToQtype(msg)
	switch qType {
	case D.TypeHTTPS:
		return fmt.Sprintf("[%s] %s", msgToHTTPSRRInfo(msg), qTypeStr)
	default:
		return fmt.Sprintf("%s %s", msgToIP(msg), qTypeStr)
	}
}

func batchExchange(ctx context.Context, clients []dnsClient, m *D.Msg) (msg *D.Msg, cache bool, err error) {
	cache = true
	fast, ctx := picker.WithTimeout[*D.Msg](ctx, resolver.DefaultDNSTimeout)
	defer fast.Close()
	domain := msgToDomain(m)
	_, qTypeStr := msgToQtype(m)
	for _, client := range clients {
		if _, isRCodeClient := client.(rcodeClient); isRCodeClient {
			msg, err = client.ExchangeContext(ctx, m)
			return msg, false, err
		}
		client := client // shadow define client to ensure the value captured by the closure will not be changed in the next loop
		fast.Go(func() (*D.Msg, error) {
			if queryRoute(ctx) == nil || log.DNSDebugEnabled() {
				log.Debugln("[DNS] resolve %s %s from %s", domain, qTypeStr, client.Address())
			}
			m, err := client.ExchangeContext(ctx, m)
			if err == nil {
				// Even an unoptimized fallback can arrive early and wait for
				// main selection. Its lifetime starts here, not when selected.
				recordDNSAnswerReceived(ctx, m, time.Now())
			}
			if err != nil {
				return nil, err
			} else if cache && (m.Rcode == D.RcodeServerFailure || m.Rcode == D.RcodeRefused) {
				// currently, cache indicates whether this msg was from a RCode client,
				// so we would ignore RCode errors from RCode clients.
				return nil, errors.New("server failure: " + D.RcodeToString[m.Rcode])
			}
			if queryRoute(ctx) == nil || log.DNSDebugEnabled() {
				log.Debugln("[DNS] %s --> %s from %s", domain, msgToLogString(m), client.Address())
			}
			return m, nil
		})
	}

	msg = fast.Wait()
	if msg == nil {
		err = errors.New("all DNS requests failed")
		if fErr := fast.Error(); fErr != nil {
			err = fmt.Errorf("%w, first error: %w", err, fErr)
		}
	}
	return
}

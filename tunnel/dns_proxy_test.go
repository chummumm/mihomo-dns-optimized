package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/common/utils"
	"github.com/metacubex/mihomo/component/geodata"
	"github.com/metacubex/mihomo/component/geodata/router"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
	R "github.com/metacubex/mihomo/rules"
	RC "github.com/metacubex/mihomo/rules/common"
	RP "github.com/metacubex/mihomo/rules/provider"
	"github.com/metacubex/mihomo/rules/wrapper"

	"github.com/miekg/dns"
	"google.golang.org/protobuf/proto"
)

func dnsProxyTestState(t *testing.T) {
	t.Helper()
	oldRules, oldSubRules, oldProviders, oldProxies, oldMode := rules, subRules, ruleProviders, proxies, mode
	rules, subRules, ruleProviders = nil, make(map[string][]C.Rule), make(map[string]P.RuleProvider)
	proxies = make(map[string]C.Proxy)
	for _, name := range []string{"DIRECT", "GLOBAL", "AWS", "DMIT", "Philippines", "wrong"} {
		proxies[name] = newDNSProxyTestProxy(newDNSProxyTestBase(name, C.Direct, true))
	}
	mode = Rule
	t.Cleanup(func() {
		rules, subRules, ruleProviders, proxies, mode = oldRules, oldSubRules, oldProviders, oldProxies, oldMode
	})
}

func dnsProxyTestRule(t *testing.T, text string) C.Rule {
	t.Helper()
	tp, payload, target, params := RC.ParseRulePayload(text, true)
	rule, err := R.ParseRule(tp, payload, target, params, subRules)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return wrapper.NewRuleWrapper(rule)
}

func dnsProxyTestQuery(t *testing.T, name string) []byte {
	t.Helper()
	message := new(dns.Msg).SetQuestion(name, dns.TypeAAAA)
	message.Id = 0x4321
	message.SetEdns0(1232, true)
	wire, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func dnsProxyTestReply(t *testing.T, wire []byte) []byte {
	t.Helper()
	request := new(dns.Msg)
	if err := request.Unpack(wire); err != nil {
		t.Fatal(err)
	}
	response, err := new(dns.Msg).SetReply(request).Pack()
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func dnsProxyTestResolver(network C.NetWork) *C.Metadata {
	return &C.Metadata{
		NetWork: network, Type: C.SOCKS5,
		DstIP: netip.MustParseAddr("8.8.8.8"), DstPort: 53,
		SrcIP: netip.MustParseAddr("127.0.0.1"), SrcPort: 53000,
		InPort: 7894, InUser: "smartdns", Process: "smartdns",
	}
}

func TestDNSProxyDomainRulesAndProviders(t *testing.T) {
	dnsProxyTestState(t)
	ruleProviders["domain"] = RP.NewInlineProvider("domain", P.Domain, []string{"+.provider.example"}, R.ParseRule)
	ruleProviders["classical"] = RP.NewInlineProvider("classical", P.Classical, []string{
		"IP-CIDR,8.8.8.8/32", "DST-PORT,53", "NETWORK,udp", "DOMAIN-SUFFIX,classical.example",
	}, R.ParseRule)
	for _, text := range []string{
		"IP-CIDR,8.8.8.8/32,wrong",
		"SRC-IP-CIDR,127.0.0.0/8,wrong",
		"DST-PORT,53,wrong",
		"NETWORK,udp,wrong",
		"PROCESS-NAME,smartdns,wrong",
		"IN-USER,smartdns,wrong",
		"NOT,((IP-CIDR,10.0.0.0/8)),wrong",
		"DOMAIN,api.anthropic.com,Philippines",
		"DOMAIN-SUFFIX,claude.ai,Philippines",
		"DOMAIN-KEYWORD,keyword,AWS",
		`DOMAIN-REGEX,^regex\.[^.]+$,DMIT`,
		"DOMAIN-WILDCARD,*.wild.example,AWS",
		"RULE-SET,domain,Philippines",
		"RULE-SET,classical,AWS",
		"MATCH,DIRECT",
	} {
		rules = append(rules, dnsProxyTestRule(t, text))
	}
	for _, test := range []struct{ host, want string }{
		{"api.anthropic.com", "Philippines"}, {"claude.ai", "Philippines"}, {"www.claude.ai", "Philippines"},
		{"contains-keyword.test", "AWS"}, {"regex.test", "DMIT"}, {"a.wild.example", "AWS"},
		{"api.provider.example", "Philippines"}, {"api.classical.example", "AWS"}, {"unmatched.test", "DIRECT"},
	} {
		t.Run(test.host, func(t *testing.T) {
			resolver := dnsProxyTestResolver(C.UDP)
			query := dnsProxyTestQuery(t, test.host+".")
			_, err := exchangeDNSProxy(context.Background(), query, resolver, selectDNSProxy,
				func(ctx context.Context, sent []byte, metadata *C.Metadata, route dnsProxyRoute) ([]byte, error) {
					if route.proxy.Name() != test.want {
						t.Errorf("selected %s, want %s", route.proxy.Name(), test.want)
					}
					return dnsProxyTestReply(t, sent), nil
				})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDNSProxyLogicalUnknownIsNotFalse(t *testing.T) {
	dnsProxyTestState(t)
	ruleProviders["mixed"] = RP.NewInlineProvider("mixed", P.Classical, []string{
		"IP-CIDR,10.0.0.0/8", "NOT,((DST-PORT,443))", "DOMAIN,listed.example",
	}, R.ParseRule)
	for _, test := range []struct {
		text, host   string
		known, match bool
	}{
		{"NOT,((IP-CIDR,10.0.0.0/8)),AWS", "a.example", false, false},
		{"AND,((DOMAIN,a.example),(DST-PORT,443)),AWS", "a.example", false, false},
		{"AND,((DOMAIN,a.example),(DST-PORT,443)),AWS", "b.example", true, false},
		{"OR,((DOMAIN,a.example),(DST-PORT,443)),AWS", "a.example", true, true},
		{"OR,((DOMAIN,a.example),(DST-PORT,443)),AWS", "b.example", false, false},
		{"AND,((DOMAIN,a.example),(NOT,((DOMAIN,b.example)))),AWS", "a.example", true, true},
		{"RULE-SET,mixed,AWS", "listed.example", true, true},
		{"RULE-SET,mixed,AWS", "unlisted.example", false, false},
		{"NOT,((RULE-SET,mixed)),AWS", "listed.example", true, false},
		{"NOT,((RULE-SET,mixed)),AWS", "unlisted.example", false, false},
	} {
		t.Run(test.text+"/"+test.host, func(t *testing.T) {
			result := matchDNSProxyRule(dnsProxyTestRule(t, test.text), &C.Metadata{Host: test.host}, 0)
			if result.known != test.known || result.match != test.match {
				t.Fatalf("got known=%v match=%v, want known=%v match=%v", result.known, result.match, test.known, test.match)
			}
		})
	}
}

func TestDNSProxySubRulesAndDisabledRules(t *testing.T) {
	dnsProxyTestState(t)
	subRules["services"] = []C.Rule{
		dnsProxyTestRule(t, "DST-PORT,53,wrong"),
		dnsProxyTestRule(t, "DOMAIN,api.example,AWS"),
		dnsProxyTestRule(t, "MATCH,DMIT"),
	}
	disabled := dnsProxyTestRule(t, "DOMAIN,api.example,wrong").(C.RuleWrapper)
	disabled.SetDisabled(true)
	rules = []C.Rule{disabled, dnsProxyTestRule(t, "SUB-RULE,(DOMAIN-SUFFIX,example),services"), dnsProxyTestRule(t, "MATCH,DIRECT")}
	for host, want := range map[string]string{"api.example": "AWS", "other.example": "DMIT", "outside.test": "DIRECT"} {
		route, err := selectDNSProxy(&C.Metadata{Host: host, NetWork: C.TCP})
		if err != nil || route.proxy.Name() != want {
			t.Fatalf("%s: got %+v (%v), want %s", host, route, err, want)
		}
	}
}

func TestDNSProxySubRulePassControlFlow(t *testing.T) {
	dnsProxyTestState(t)
	proxies["PASS"] = newDNSProxyTestProxy(newDNSProxyTestBase("PASS", C.Pass, true))
	proxies["PASS-RULE"] = newDNSProxyTestProxy(newDNSProxyTestBase("PASS-RULE", C.PassRule, true))
	for _, test := range []struct{ control, want string }{
		{"PASS", "DIRECT"},
		{"PASS-RULE", "Philippines"},
	} {
		t.Run(test.control, func(t *testing.T) {
			subRules["services"] = []C.Rule{
				dnsProxyTestRule(t, "DOMAIN,a.example,"+test.control),
				dnsProxyTestRule(t, "MATCH,Philippines"),
			}
			rules = []C.Rule{
				dnsProxyTestRule(t, "SUB-RULE,(DOMAIN-SUFFIX,example),services"),
				dnsProxyTestRule(t, "MATCH,DIRECT"),
			}
			route, err := selectDNSProxy(&C.Metadata{Host: "a.example", NetWork: C.TCP})
			if err != nil || route.proxy.Name() != test.want {
				t.Fatalf("%s: got %+v (%v), want %s", test.control, route, err, test.want)
			}
			ordinary, _, err := match(&C.Metadata{Host: "a.example", NetWork: C.TCP}, C.RuleMatchHelper{})
			if err != nil || ordinary.Name() != route.proxy.Name() {
				t.Fatalf("%s: DNS route differs from ordinary routing: %+v, %v", test.control, ordinary, err)
			}
		})
	}
	// PASS-RULE is a sub-rule control action. At top level it must not turn
	// into PASS and silently continue to a later DIRECT rule.
	rules = []C.Rule{dnsProxyTestRule(t, "DOMAIN,a.example,PASS-RULE"), dnsProxyTestRule(t, "MATCH,DIRECT")}
	route, err := selectDNSProxy(&C.Metadata{Host: "a.example", NetWork: C.TCP})
	if err != nil || route.proxy.Type() != C.PassRule {
		t.Fatalf("top-level PASS-RULE incorrectly continued: %+v, %v", route, err)
	}
}

func TestDNSProxyGeosite(t *testing.T) {
	dnsProxyTestState(t)
	oldHome := C.Path.HomeDir()
	C.SetHomeDir(t.TempDir())
	geodata.ClearGeoSiteCache()
	t.Cleanup(func() { C.SetHomeDir(oldHome); geodata.ClearGeoSiteCache() })
	data, err := proto.Marshal(&router.GeoSiteList{Entry: []*router.GeoSite{
		{CountryCode: "CN", Domain: []*router.Domain{{Type: router.Domain_Domain, Value: "cn.example"}}},
		{CountryCode: "dns-proxy-test", Domain: []*router.Domain{{Type: router.Domain_Domain, Value: "geosite.example"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(C.Path.GeoSite(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	rules = []C.Rule{dnsProxyTestRule(t, "GEOSITE,dns-proxy-test,Philippines"), dnsProxyTestRule(t, "MATCH,DIRECT")}
	route, err := selectDNSProxy(&C.Metadata{Host: "api.geosite.example", NetWork: C.UDP})
	if err != nil || route.proxy.Name() != "Philippines" {
		t.Fatalf("GEOSITE did not select Philippines: %+v, %v", route, err)
	}
}

func TestDNSProxyModeAndNoUDPFallback(t *testing.T) {
	dnsProxyTestState(t)
	rules = []C.Rule{dnsProxyTestRule(t, "MATCH,AWS")}
	for value, want := range map[TunnelMode]string{Direct: "DIRECT", Global: "GLOBAL", Rule: "AWS"} {
		mode = value
		route, err := selectDNSProxy(&C.Metadata{Host: "example.com", NetWork: C.UDP})
		if err != nil || route.proxy.Name() != want {
			t.Fatalf("%s: got %+v, %v; want %s", value, route, err, want)
		}
	}
	mode = Rule
	proxies["AWS"] = newDNSProxyTestProxy(newDNSProxyTestBase("AWS", C.Http, false))
	if _, err := selectDNSProxy(&C.Metadata{Host: "example.com", NetWork: C.UDP}); err == nil {
		t.Fatal("UDP-unsupported selected node must fail, not fall back to DIRECT")
	}
}

type dnsProxyTestProxy struct{ C.ProxyAdapter }

func newDNSProxyTestProxy(adapter C.ProxyAdapter) C.Proxy {
	return &dnsProxyTestProxy{ProxyAdapter: adapter}
}
func (p *dnsProxyTestProxy) Adapter() C.ProxyAdapter                      { return p.ProxyAdapter }
func (p *dnsProxyTestProxy) AliveForTestUrl(string) bool                  { return true }
func (p *dnsProxyTestProxy) DelayHistory() []C.DelayHistory               { return nil }
func (p *dnsProxyTestProxy) ExtraDelayHistories() map[string]C.ProxyState { return nil }
func (p *dnsProxyTestProxy) LastDelayForTestUrl(string) uint16            { return 0 }
func (p *dnsProxyTestProxy) URLTest(context.Context, string, utils.IntRanges[uint16]) (uint16, error) {
	return 0, C.ErrNotSupport
}

type dnsProxyTestBase struct {
	name string
	tp   C.AdapterType
	udp  bool
}

func newDNSProxyTestBase(name string, tp C.AdapterType, udp bool) *dnsProxyTestBase {
	return &dnsProxyTestBase{name: name, tp: tp, udp: udp}
}
func (a *dnsProxyTestBase) Name() string                 { return a.name }
func (a *dnsProxyTestBase) Type() C.AdapterType          { return a.tp }
func (a *dnsProxyTestBase) Addr() string                 { return "" }
func (a *dnsProxyTestBase) SupportUDP() bool             { return a.udp }
func (a *dnsProxyTestBase) ProxyInfo() C.ProxyInfo       { return C.ProxyInfo{} }
func (a *dnsProxyTestBase) MarshalJSON() ([]byte, error) { return []byte("{}"), nil }
func (a *dnsProxyTestBase) DialContext(context.Context, *C.Metadata) (C.Conn, error) {
	return nil, C.ErrNotSupport
}
func (a *dnsProxyTestBase) ListenPacketContext(context.Context, *C.Metadata) (C.PacketConn, error) {
	return nil, C.ErrNotSupport
}
func (a *dnsProxyTestBase) SupportUOT() bool                 { return false }
func (a *dnsProxyTestBase) IsL3Protocol(*C.Metadata) bool    { return false }
func (a *dnsProxyTestBase) Unwrap(*C.Metadata, bool) C.Proxy { return nil }
func (a *dnsProxyTestBase) Close() error                     { return nil }

type dnsProxyTestConnection struct{ chain C.Chain }

func (c *dnsProxyTestConnection) Chains() C.Chain         { return c.chain }
func (c *dnsProxyTestConnection) ProviderChains() C.Chain { return nil }
func (c *dnsProxyTestConnection) AppendToChains(a C.ProxyAdapter) {
	c.chain = append(c.chain, a.Name())
}
func (c *dnsProxyTestConnection) RemoteDestination() string { return "" }

type dnsProxyTestConn struct {
	N.ExtendedConn
	dnsProxyTestConnection
}
type dnsProxyTestProxyPacketConn struct {
	N.EnhancePacketConn
	dnsProxyTestConnection
}

func (c *dnsProxyTestProxyPacketConn) ResolveUDP(context.Context, *C.Metadata) error { return nil }

type dnsProxyTestAdapter struct {
	*dnsProxyTestBase
	choose func(*C.Metadata, bool) C.Proxy
	dial   func(context.Context, *C.Metadata) (C.Conn, error)
	packet func(context.Context, *C.Metadata) (C.PacketConn, error)
}

func (a *dnsProxyTestAdapter) Unwrap(metadata *C.Metadata, touch bool) C.Proxy {
	if a.choose != nil {
		return a.choose(metadata, touch)
	}
	return nil
}

func (a *dnsProxyTestAdapter) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	return a.dial(ctx, metadata)
}

func (a *dnsProxyTestAdapter) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (C.PacketConn, error) {
	return a.packet(ctx, metadata)
}

func TestDNSProxyEachQuerySelectsLiveGroupButDialsOriginalResolver(t *testing.T) {
	dnsProxyTestState(t)
	selected := "AWS"
	var seen []string
	group := &dnsProxyTestAdapter{dnsProxyTestBase: newDNSProxyTestBase("live", C.Selector, true)}
	group.choose = func(metadata *C.Metadata, touch bool) C.Proxy {
		if metadata.DstIP.IsValid() || metadata.DstPort != 0 || metadata.SrcIP.IsValid() || metadata.InUser != "" {
			t.Errorf("resolver metadata leaked into group selection: %+v", metadata)
		}
		seen = append(seen, metadata.Host)
		return proxies[selected]
	}
	proxies["live"] = newDNSProxyTestProxy(group)
	rules = []C.Rule{dnsProxyTestRule(t, "MATCH,live")}
	for _, network := range []C.NetWork{C.UDP, C.TCP} {
		resolver := dnsProxyTestResolver(network)
		original := resolver.Clone()
		for _, host := range []string{"First.Example.", "SECOND.Example."} {
			selected = map[string]string{"First.Example.": "AWS", "SECOND.Example.": "Philippines"}[host]
			query := dnsProxyTestQuery(t, host)
			_, err := exchangeDNSProxy(context.Background(), query, resolver, selectDNSProxy,
				func(ctx context.Context, sent []byte, destination *C.Metadata, route dnsProxyRoute) ([]byte, error) {
					if route.proxy.Name() != selected {
						t.Errorf("stale selected group: got %s, want %s", route.proxy.Name(), selected)
					}
					if destination.Host != "" || destination.SniffHost != "" || destination.RemoteAddress() != "8.8.8.8:53" || destination.NetWork != network {
						t.Errorf("wrong transport destination: %+v", destination)
					}
					if !bytes.Equal(sent, query) {
						t.Error("query wire/ID/question/EDNS changed")
					}
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > dnsProxyTimeout {
						t.Error("exchange does not have a bounded deadline")
					}
					return dnsProxyTestReply(t, sent), nil
				})
			if err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(original, resolver) {
			t.Error("caller metadata mutated")
		}
	}
	if !reflect.DeepEqual(seen, []string{"first.example", "second.example", "first.example", "second.example"}) {
		t.Fatalf("must choose once per question using normalized QNAME, got %v", seen)
	}
}

func TestDNSProxyRejectsNonDNSAndWrongDestinations(t *testing.T) {
	query := dnsProxyTestQuery(t, "example.com.")
	mutate := func(change func(*dns.Msg)) []byte {
		message := new(dns.Msg)
		_ = message.Unpack(query)
		change(message)
		wire, err := message.Pack()
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	for name, wire := range map[string][]byte{
		"empty": nil, "short": []byte("GET / HTTP/1.1"), "trailing": append(append([]byte(nil), query...), 0),
		"response":           mutate(func(m *dns.Msg) { m.Response = true }),
		"update":             mutate(func(m *dns.Msg) { m.Opcode = dns.OpcodeUpdate }),
		"multiple questions": mutate(func(m *dns.Msg) { m.Question = append(m.Question, m.Question[0]) }),
		"no questions":       mutate(func(m *dns.Msg) { m.Question = nil }),
		"zone transfer":      mutate(func(m *dns.Msg) { m.Question[0].Qtype = dns.TypeAXFR }),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := exchangeDNSProxy(context.Background(), wire, dnsProxyTestResolver(C.UDP),
				func(*C.Metadata) (dnsProxyRoute, error) {
					t.Fatal("invalid input reached router")
					return dnsProxyRoute{}, nil
				}, nil)
			if err == nil {
				t.Fatal("invalid DNS accepted")
			}
		})
	}
	for name, change := range map[string]func(*C.Metadata){
		"HTTPS":              func(m *C.Metadata) { m.DstPort = 443 },
		"DoT":                func(m *C.Metadata) { m.DstPort = 853 },
		"hostname bootstrap": func(m *C.Metadata) { m.Host = "dns.example.com"; m.DstIP = netip.Addr{} },
		"multicast":          func(m *C.Metadata) { m.DstIP = netip.MustParseAddr("ff02::fb") },
		"broadcast":          func(m *C.Metadata) { m.DstIP = netip.MustParseAddr("255.255.255.255") },
		"unspecified":        func(m *C.Metadata) { m.DstIP = netip.IPv4Unspecified() },
		"invalid network":    func(m *C.Metadata) { m.NetWork = C.InvalidNet },
	} {
		t.Run(name, func(t *testing.T) {
			resolver := dnsProxyTestResolver(C.UDP)
			change(resolver)
			if _, err := exchangeDNSProxy(context.Background(), query, resolver, nil, nil); err == nil {
				t.Fatal("invalid resolver target accepted")
			}
		})
	}
}

func TestDNSProxyResponseValidationAndRejection(t *testing.T) {
	dnsProxyTestState(t)
	query := dnsProxyTestQuery(t, "example.com.")
	request := new(dns.Msg)
	_ = request.Unpack(query)
	for name, change := range map[string]func(*dns.Msg){
		"ID": func(m *dns.Msg) { m.Id++ }, "QR": func(m *dns.Msg) { m.Response = false },
		"name":   func(m *dns.Msg) { m.Question[0].Name = "elsewhere.example." },
		"type":   func(m *dns.Msg) { m.Question[0].Qtype = dns.TypeA },
		"class":  func(m *dns.Msg) { m.Question[0].Qclass = dns.ClassCHAOS },
		"opcode": func(m *dns.Msg) { m.Opcode = dns.OpcodeUpdate },
	} {
		t.Run(name, func(t *testing.T) {
			response := new(dns.Msg).SetReply(request.Copy())
			change(response)
			wire, err := response.Pack()
			if err != nil {
				t.Fatal(err)
			}
			if err := validateDNSProxyResponse(request, wire); err == nil {
				t.Fatal("mismatched reply accepted")
			}
		})
	}
	for _, reject := range []C.ProxyAdapter{newDNSProxyTestBase("REJECT", C.Reject, true), newDNSProxyTestBase("REJECT-DROP", C.RejectDrop, true)} {
		proxies[reject.Name()] = newDNSProxyTestProxy(reject)
		rules = []C.Rule{dnsProxyTestRule(t, "MATCH,"+reject.Name())}
		response, err := exchangeDNSProxy(context.Background(), query, dnsProxyTestResolver(C.UDP), selectDNSProxy,
			func(context.Context, []byte, *C.Metadata, dnsProxyRoute) ([]byte, error) {
				t.Fatal("rejection contacted upstream")
				return nil, nil
			})
		if reject.Type() == C.RejectDrop {
			if !errors.Is(err, errDNSProxyDrop) || response != nil {
				t.Fatal("REJECT-DROP did not drop")
			}
		} else {
			message := new(dns.Msg)
			if err != nil || message.Unpack(response) != nil || message.Rcode != dns.RcodeRefused || message.Id != request.Id {
				t.Fatalf("REJECT did not return matching REFUSED: %v", err)
			}
		}
	}
}

func TestDNSProxyTCPWireAndCancellation(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(map[bool]string{false: "framing", true: "cancellation"}[cancelEarly], func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			query := dnsProxyTestQuery(t, "api.anthropic.com.")
			response := dnsProxyTestReply(t, query)
			readQuery := make(chan []byte, 1)
			go func() {
				var size [2]byte
				if _, err := io.ReadFull(server, size[:]); err != nil {
					readQuery <- nil
					return
				}
				wire := make([]byte, binary.BigEndian.Uint16(size[:]))
				if _, err := io.ReadFull(server, wire); err != nil {
					readQuery <- nil
					return
				}
				readQuery <- wire
				if !cancelEarly {
					binary.BigEndian.PutUint16(size[:], uint16(len(response)))
					// Fragment the frame deliberately to exercise io.ReadFull.
					_, _ = server.Write(size[:1])
					_, _ = server.Write(size[1:])
					_, _ = server.Write(response)
				}
			}()
			base := &dnsProxyTestAdapter{dnsProxyTestBase: newDNSProxyTestBase("wire", C.Direct, true)}
			base.dial = func(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
				if metadata.Host != "" || metadata.RemoteAddress() != "8.8.8.8:53" {
					t.Error("incorrect dial destination")
				}
				return &dnsProxyTestConn{ExtendedConn: N.NewExtendedConn(client)}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			type result struct {
				wire []byte
				err  error
			}
			resultCh := make(chan result, 1)
			go func() {
				wire, err := exchangeDNSProxyWire(ctx, query, dnsProxyTestResolver(C.TCP), dnsProxyRoute{proxy: newDNSProxyTestProxy(base)})
				resultCh <- result{wire, err}
			}()
			if got := <-readQuery; !bytes.Equal(got, query) {
				t.Fatal("TCP query was changed or unframed")
			}
			if cancelEarly {
				cancel()
			}
			select {
			case got := <-resultCh:
				if cancelEarly {
					if got.err == nil {
						t.Fatal("cancellation did not interrupt read")
					}
				} else if got.err != nil || !bytes.Equal(got.wire, response) {
					t.Fatalf("TCP reply: %x, %v", got.wire, got.err)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("exchange did not complete or respect cancellation promptly")
			}
		})
	}
}

type dnsProxyTestDatagram struct {
	wire    []byte
	address net.Addr
}
type dnsProxyTestPacketConn struct {
	reads  chan dnsProxyTestDatagram
	writes chan dnsProxyTestDatagram
	closed chan struct{}
	once   sync.Once
}

func (c *dnsProxyTestPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case packet := <-c.reads:
		return copy(p, packet.wire), packet.address, nil
	case <-c.closed:
		return 0, nil, net.ErrClosed
	}
}
func (c *dnsProxyTestPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.writes <- dnsProxyTestDatagram{append([]byte(nil), p...), addr}
	return len(p), nil
}
func (c *dnsProxyTestPacketConn) Close() error { c.once.Do(func() { close(c.closed) }); return nil }
func (c *dnsProxyTestPacketConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
}
func (c *dnsProxyTestPacketConn) SetDeadline(time.Time) error      { return nil }
func (c *dnsProxyTestPacketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *dnsProxyTestPacketConn) SetWriteDeadline(time.Time) error { return nil }

func TestDNSProxyUDPWireRejectsOtherResponseSources(t *testing.T) {
	query := dnsProxyTestQuery(t, "claude.ai.")
	response := dnsProxyTestReply(t, query)
	packets := &dnsProxyTestPacketConn{reads: make(chan dnsProxyTestDatagram, 2), writes: make(chan dnsProxyTestDatagram, 1), closed: make(chan struct{})}
	packets.reads <- dnsProxyTestDatagram{[]byte("unexpected"), &net.UDPAddr{IP: net.IPv4(1, 1, 1, 1), Port: 53}}
	packets.reads <- dnsProxyTestDatagram{response, &net.UDPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 53}}
	base := &dnsProxyTestAdapter{dnsProxyTestBase: newDNSProxyTestBase("wire", C.Direct, true)}
	base.packet = func(ctx context.Context, metadata *C.Metadata) (C.PacketConn, error) {
		return &dnsProxyTestProxyPacketConn{EnhancePacketConn: N.NewEnhancePacketConn(packets)}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := exchangeDNSProxyWire(ctx, query, dnsProxyTestResolver(C.UDP), dnsProxyRoute{proxy: newDNSProxyTestProxy(base)})
	if err != nil || !bytes.Equal(got, response) {
		t.Fatalf("UDP reply: %x, %v", got, err)
	}
	packet := <-packets.writes
	if !bytes.Equal(packet.wire, query) || packet.address.String() != "8.8.8.8:53" {
		t.Fatalf("UDP destination/query changed: %+v", packet)
	}
}

func TestDNSProxyIPv6ResolverAndEndpoint(t *testing.T) {
	dnsProxyTestState(t)
	rules = []C.Rule{dnsProxyTestRule(t, "MATCH,DIRECT")}
	resolver := dnsProxyTestResolver(C.UDP)
	resolver.DstIP = netip.MustParseAddr("2606:4700:4700::1111")
	_, err := exchangeDNSProxy(context.Background(), dnsProxyTestQuery(t, "example.com."), resolver, selectDNSProxy,
		func(ctx context.Context, wire []byte, metadata *C.Metadata, route dnsProxyRoute) ([]byte, error) {
			if metadata.RemoteAddress() != "[2606:4700:4700::1111]:53" || !sameDNSProxyEndpoint(metadata.UDPAddr(), resolver.AddrPort()) {
				t.Fatal("IPv6 resolver corrupted")
			}
			return dnsProxyTestReply(t, wire), nil
		})
	if err != nil {
		t.Fatal(err)
	}
}

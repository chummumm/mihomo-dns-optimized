package tunnel

import (
	"fmt"
	C "github.com/metacubex/mihomo/constant"
	RC "github.com/metacubex/mihomo/rules/common"
	"net/netip"
	"testing"
)

type dnsPerfRules struct{ list []C.Rule }

func (d *dnsPerfRules) Rules() []C.Rule { return d.list }

func TestDNSPerfCompiledEquivalent(t *testing.T) {
	dnsProxyTestState(t)
	texts := []string{
		"DOMAIN-SUFFIX,example.com,DIRECT",
		"AND,((SRC-IP-CIDR,192.168.3.1/32),(NOT,((DST-PORT,53)))),DIRECT",
		"OR,((DOMAIN,dns.weibo.cn),(AND,((NETWORK,UDP),(DST-PORT,443)))),REJECT",
		"NOT,((IP-CIDR,10.0.0.0/8)),DIRECT",
		"OR,((IP-CIDR,10.0.0.0/8),(DOMAIN-SUFFIX,example.com)),DIRECT",
		"AND,((IP-CIDR,10.0.0.0/8),(DOMAIN-SUFFIX,example.com)),DIRECT",
		"AND,((IN-NAME,DNS),(SRC-IP-SUFFIX,::78ef/16)),DIRECT",
	}
	for _, text := range texts {
		rule := dnsProxyTestRule(t, text)
		compiled := compileDNSNode(rule, 0)
		for _, host := range []string{"www.example.com", "other.net", "dns.weibo.cn"} {
			for _, source := range []string{"192.168.3.1", "192.168.3.3", "2001:db8::78ef"} {
				for _, network := range []C.NetWork{C.TCP, C.UDP} {
					md := &C.Metadata{Host: host, SniffHost: host, SrcIP: netip.MustParseAddr(source), DstPort: 53, NetWork: network, InName: "DNS"}
					want := matchDNSProxyRuleContext(rule, md.Clone(), dnsRuleContext{}, 0)
					got := compiled.eval(md.Clone(), dnsRuleContext{}, 0)
					if got != want {
						t.Fatalf("%s host=%s source=%s: got=%+v want=%+v", text, host, source, got, want)
					}
				}
			}
		}
	}
}

func TestDNSPerfClassicalIndexEquivalent(t *testing.T) {
	list := make([]C.Rule, 0, 1001)
	for i := 0; i < 1000; i++ {
		list = append(list, RC.NewDomainSuffix(fmt.Sprintf("d%d.example", i), "DIRECT"))
	}
	ip, err := RC.NewIPCIDR("10.0.0.0/8", "DIRECT", RC.WithIPCIDRNoResolve(true))
	if err != nil {
		t.Fatal(err)
	}
	list = append(list, ip)
	strategy := &dnsPerfRules{list}
	for _, host := range []string{"d0.example", "sub.d999.example", "other.net", "D0.EXAMPLE", "bad-d0.example"} {
		md := &C.Metadata{Host: host, DstPort: 53}
		want := matchDNSProxyChildrenContext(list, C.OR, md.Clone(), dnsRuleContext{}, 1)
		got := matchDNSClassical(strategy, md.Clone(), dnsRuleContext{}, 1)
		if got.match != want.match || got.known != want.known {
			t.Fatalf("%s: got=%+v want=%+v", host, got, want)
		}
	}
	// A provider update is a new immutable strategy, never an old cached view.
	updated := &dnsPerfRules{[]C.Rule{RC.NewDomain("other.net", "DIRECT")}}
	got := matchDNSClassical(updated, &C.Metadata{Host: "other.net", DstPort: 53}, dnsRuleContext{}, 1)
	if !got.known || !got.match {
		t.Fatal("provider generation reused old index")
	}
}

func BenchmarkDNSPerfClassicalMiss(b *testing.B) {
	list := make([]C.Rule, 0, 10000)
	for i := 0; i < 10000; i++ {
		list = append(list, RC.NewDomainSuffix(fmt.Sprintf("d%d.example", i), "DIRECT"))
	}
	strategy := &dnsPerfRules{list}
	md := &C.Metadata{Host: "absent.example.net", DstPort: 53}
	matchDNSClassical(strategy, md, dnsRuleContext{}, 1)
	b.Run("linear-reference", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if matchDNSProxyChildrenContext(list, C.OR, md, dnsRuleContext{}, 1).match {
				b.Fatal("unexpected match")
			}
		}
	})
	b.Run("indexed", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if matchDNSClassical(strategy, md, dnsRuleContext{}, 1).match {
				b.Fatal("unexpected match")
			}
		}
	})
}

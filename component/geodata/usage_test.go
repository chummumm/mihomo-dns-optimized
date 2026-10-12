package geodata

import (
	"testing"

	C "github.com/metacubex/mihomo/constant"
)

type usageTestRule struct {
	typeID C.RuleType
	text   string
}

func (r usageTestRule) RuleType() C.RuleType                                { return r.typeID }
func (r usageTestRule) Match(*C.Metadata, C.RuleMatchHelper) (bool, string) { return false, "" }
func (r usageTestRule) Adapter() string                                     { return "" }
func (r usageTestRule) Payload() string                                     { return r.text }
func (r usageTestRule) ProviderNames() []string                             { return nil }

type usageTestGroup struct{ rules []C.Rule }

func (g usageTestGroup) Rules() []C.Rule { return g.rules }

type usageTestWrapper struct {
	usageTestRule
	inner C.Rule
}

func (w usageTestWrapper) Unwrap() C.Rule { return w.inner }

func TestRuleUsageReadsCompiledObjectsWithoutGlobalSideEffects(t *testing.T) {
	previous := CurrentUsage()
	t.Cleanup(func() { SetUsage(previous) })
	SetUsage(Usage{GeoIP: true, GeoSite: true, ASN: true})
	if got := RuleUsage(usageTestRule{typeID: C.Domain}); got != (Usage{}) {
		t.Fatalf("unrelated global InitGeo flags contaminated rule usage: %+v", got)
	}
	if got := RuleUsage(usageTestRule{typeID: C.GEOIP, text: "LAN"}); got != (Usage{}) {
		t.Fatalf("LAN pseudo rule enabled a remote database: %+v", got)
	}
	group := usageTestGroup{rules: []C.Rule{
		usageTestWrapper{inner: usageTestRule{typeID: C.GEOSITE, text: "cn"}},
		usageTestRule{typeID: C.SrcGEOIP, text: "cn"},
		usageTestRule{typeID: C.SrcIPASN, text: "64512"},
	}}
	if got := RuleUsage(group); got != (Usage{GeoIP: true, GeoSite: true, ASN: true}) {
		t.Fatalf("wrapped/compound rule usage = %+v", got)
	}
}

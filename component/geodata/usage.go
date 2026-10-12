package geodata

import (
	"strings"

	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
)

// Usage describes the GeoData families referenced by one parsed configuration.
// Sources alone do not enable a family: the default YAML supplies all four
// sources even when no rule or DNS policy uses any of them.
type Usage struct {
	GeoIP   bool
	GeoSite bool
	ASN     bool
}

func CurrentUsage() Usage {
	return Usage{GeoIP: GeoIpEnable(), GeoSite: GeoSiteEnable(), ASN: ASNEnable()}
}

func SetUsage(usage Usage) {
	geoIpEnable.Store(usage.GeoIP)
	geoSiteEnable.Store(usage.GeoSite)
	asnEnable.Store(usage.ASN)
}

func (u Usage) Merge(other Usage) Usage {
	return Usage{GeoIP: u.GeoIP || other.GeoIP, GeoSite: u.GeoSite || other.GeoSite, ASN: u.ASN || other.ASN}
}

// RuleUsage inspects immutable, successfully parsed rule objects instead of
// global InitGeo* flags. A rejected candidate parse or a provider being parsed
// concurrently therefore cannot change which active databases get updated.
func RuleUsage(value any) Usage {
	return ruleUsage(value, 0)
}

func ruleUsage(value any, depth int) Usage {
	if value == nil || depth > 64 {
		return Usage{}
	}
	var usage Usage
	if rule, ok := value.(C.Rule); ok {
		switch rule.RuleType() {
		case C.GEOSITE:
			usage.GeoSite = true
		case C.GEOIP, C.SrcGEOIP:
			usage.GeoIP = !strings.EqualFold(rule.Payload(), "lan")
		case C.IPASN, C.SrcIPASN:
			usage.ASN = true
		}
	}
	if wrapper, ok := value.(interface{ Unwrap() C.Rule }); ok {
		usage = usage.Merge(ruleUsage(wrapper.Unwrap(), depth+1))
	}
	if group, ok := value.(interface{ Rules() []C.Rule }); ok {
		for _, rule := range group.Rules() {
			usage = usage.Merge(ruleUsage(rule, depth+1))
		}
	}
	return usage
}

func UsageForRule(rule C.Rule) Usage {
	return RuleUsage(rule)
}

// Strategy() returns the provider's atomically published immutable strategy.
// Later successful provider updates can add or remove GeoData dependencies.
func UsageForProvider(provider P.RuleProvider) Usage {
	if provider == nil {
		return Usage{}
	}
	return RuleUsage(provider.Strategy())
}

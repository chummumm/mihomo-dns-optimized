package config

import "github.com/metacubex/mihomo/component/geodata"

// CalculateGeoDataUsage follows the actual immutable rule/matcher objects of
// one parsed config. Historical global initialization flags and a concurrently
// parsing candidate cannot add resources to it. Provider contents are merged
// separately from their latest committed strategy at update time.
func CalculateGeoDataUsage(cfg *Config) geodata.Usage {
	var usage geodata.Usage
	add := func(value any) { usage = usage.Merge(geodata.RuleUsage(value)) }
	for _, rule := range cfg.Rules {
		add(rule)
	}
	for _, rules := range cfg.SubRules {
		for _, rule := range rules {
			add(rule)
		}
	}
	if dns := cfg.DNS; dns != nil {
		if dns.Enable || dns.DNSRuleRouting {
			for _, policy := range dns.ProxyServerPolicy {
				add(policy.Matcher)
			}
		}
		if dns.Enable {
			for _, policy := range dns.NameServerPolicy {
				add(policy.Matcher)
			}
			for _, matcher := range dns.FallbackIPFilter {
				add(matcher)
			}
			for _, matcher := range dns.FallbackDomainFilter {
				add(matcher)
			}
			if skipper := dns.FakeIPSkipper; skipper != nil {
				for _, rule := range skipper.Rules {
					add(rule)
				}
				for _, matcher := range skipper.Host {
					add(matcher)
				}
			}
		}
	}
	if sniffer := cfg.Sniffer; sniffer != nil && sniffer.Enable {
		for _, matcher := range sniffer.ForceDomain {
			add(matcher)
		}
		for _, matcher := range sniffer.SkipDomain {
			add(matcher)
		}
		for _, matcher := range sniffer.SkipSrcAddress {
			add(matcher)
		}
		for _, matcher := range sniffer.SkipDstAddress {
			add(matcher)
		}
	}
	return usage
}

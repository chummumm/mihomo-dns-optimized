//go:build linux

package transparentproxy

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/metacubex/nftables"
	"github.com/metacubex/nftables/binaryutil"
	"github.com/metacubex/nftables/expr"
	"golang.org/x/sys/unix"
)

// RecoverScopes reads only working packet-classification rules. It does not
// decode a saved configuration, invent marks for orphaned routes, or treat an
// unused backend chain as evidence that its routing policy was installed.
func (f *nftFirewall) RecoverScopes(ctx context.Context) ([]captureScope, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	actual, err := f.read(ctx)
	if err != nil {
		return nil, false, err
	}
	if !hasNFTModuleObjects(actual) {
		return nil, false, nil
	}
	scopes, err := recoverNFTScopes(actual)
	return scopes, true, err
}

func recoverNFTScopes(actual *nftSnapshot) ([]captureScope, error) {
	template, err := buildNFTRuleset(firewallState{TCP: ModeOff, UDP: ModeOff})
	if err != nil {
		return nil, err
	}
	templates := make(map[string]*nftables.Chain)
	for _, chain := range template.chains {
		templates[chain.chain.Name] = chain.chain
	}
	var result []captureScope
	for _, family := range []int{4, 6} {
		suffix := fmt.Sprint(family)
		nfproto := byte(unix.NFPROTO_IPV4)
		if family == 6 {
			nfproto = unix.NFPROTO_IPV6
		}
		marks := make(map[Mode]uint32)
		ambiguous := make(map[Mode]bool)
		for _, path := range []struct{ base, entry, proxy string }{
			{"prerouting", "pre" + suffix, "tproxy" + suffix},
			{"output", "out" + suffix, "mark_tproxy" + suffix},
		} {
			base, entry := actual.chains[path.base], actual.chains[path.entry]
			if base == nil || entry == nil || !equalNFTChain(base.chain, templates[path.base]) || !equalNFTChain(entry.chain, templates[path.entry]) {
				continue
			}
			branch, err := nftRuleFingerprint(&nftables.Rule{Exprs: append(nftMetaMatch(expr.MetaKeyNFPROTO, []byte{nfproto}), nftGoto(path.entry))})
			if err != nil {
				return nil, err
			}
			reachable := false
			for _, rule := range base.rules {
				if managedNFTRule(rule, path.base) && bytes.Equal(rule.expressions, branch) {
					reachable = true
					break
				}
			}
			if !reachable {
				continue
			}
			for _, target := range []struct {
				name    string
				backend Mode
			}{{path.proxy, ModeTProxy}, {"tun" + suffix, ModeTUN}} {
				backend := actual.chains[target.name]
				if backend == nil || !equalNFTChain(backend.chain, templates[target.name]) {
					continue
				}
				branch, err := nftRuleFingerprint(&nftables.Rule{Exprs: []expr.Any{nftGoto(target.name)}})
				if err != nil {
					return nil, err
				}
				reachable = false
				for _, rule := range entry.rules {
					if !managedNFTRule(rule, path.entry) {
						continue
					}
					parts, err := parseWireAttributes(rule.expressions)
					if err != nil {
						return nil, err
					}
					if len(parts) != 0 && bytes.Equal(appendWireAttribute(nil, unix.NFTA_LIST_ELEM, parts[len(parts)-1].Data), branch) {
						reachable = true
						break
					}
				}
				if !reachable {
					continue
				}
				for _, rule := range backend.rules {
					if !managedNFTRule(rule, target.name) {
						continue
					}
					mark, err := nftTerminalMark(rule)
					if err != nil {
						return nil, err
					}
					if mark == 0 {
						continue
					}
					if previous := marks[target.backend]; previous != 0 && previous != mark {
						ambiguous[target.backend] = true
					}
					marks[target.backend] = mark
				}
			}
		}
		for backend, mark := range marks {
			if !ambiguous[backend] {
				result = append(result, captureScope{Backend: backend, Mark: mark, IPv6: family == 6})
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].IPv6 != result[j].IPv6 {
			return !result[i].IPv6
		}
		return result[i].Backend < result[j].Backend
	})
	return result, nil
}

func nftTerminalMark(rule *nftRuleState) (uint32, error) {
	parts, err := parseWireAttributes(rule.expressions)
	if err != nil || len(parts) < 3 {
		return 0, err
	}
	parts = parts[len(parts)-3:]
	outer, err := nftAttributeMap(parts[0].Data)
	if err != nil {
		return 0, err
	}
	name, err := nftStringAttribute(outer, unix.NFTA_EXPR_NAME)
	if err != nil || name != "immediate" {
		return 0, err
	}
	fields, err := nftAttributeMap(outer[unix.NFTA_EXPR_DATA])
	if err != nil {
		return 0, err
	}
	value, err := nftAttributeMap(fields[unix.NFTA_IMMEDIATE_DATA])
	if err != nil {
		return 0, err
	}
	if len(value[unix.NFTA_DATA_VALUE]) != 4 {
		return 0, nil
	}
	mark := binaryutil.NativeEndian.Uint32(value[unix.NFTA_DATA_VALUE])
	var tail []byte
	for _, part := range parts {
		tail = appendWireAttribute(tail, unix.NFTA_LIST_ELEM, part.Data)
	}
	for _, verdict := range []*expr.Verdict{nftAccept(), nftDrop()} {
		expected, err := nftRuleFingerprint(&nftables.Rule{Exprs: nftMarkVerdict(mark, verdict)})
		if err != nil {
			return 0, err
		}
		if bytes.Equal(tail, expected) {
			return mark, nil
		}
	}
	return 0, nil
}

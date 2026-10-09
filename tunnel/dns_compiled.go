package tunnel

import (
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/metacubex/mihomo/component/trie"
	C "github.com/metacubex/mihomo/constant"
)

// This is a derived execution view of the SAME rules, used by DNS only. It
// does not cache route decisions, select groups early, reorder conditions or
// modify the ordinary connection matcher. Dynamic wrappers remain dynamic.
type dnsCompiledNode struct {
	rule     C.Rule
	kind     uint8
	value    dnsProxyMatch
	children []*dnsCompiledNode
}

const (
	dnsNodeOriginal uint8 = iota
	dnsNodeLeaf
	dnsNodeConstant
	dnsNodeWrapper
	dnsNodeAnd
	dnsNodeOr
	dnsNodeNot
)

type dnsCompiledSnapshot struct{ nodes map[C.Rule]*dnsCompiledNode }

var dnsCompiled atomic.Pointer[dnsCompiledSnapshot]

func compileDNSNode(rule C.Rule, depth int) *dnsCompiledNode {
	n := &dnsCompiledNode{rule: rule}
	if depth >= dnsProxyMaxDepth {
		return n
	}
	if wrapper, ok := rule.(C.RuleWrapper); ok {
		n.kind = dnsNodeWrapper
		n.children = []*dnsCompiledNode{compileDNSNode(wrapper.Unwrap(), depth+1)}
		return n
	}
	switch rule.RuleType() {
	case C.IPCIDR, C.GEOIP, C.IPASN, C.IPSuffix:
		n.kind = dnsNodeConstant
		n.value = dnsProxyMatch{}
	case C.DstPort:
		matched, adapter := rule.Match(&C.Metadata{DstPort: 53}, C.RuleMatchHelper{})
		n.kind = dnsNodeConstant
		n.value = dnsProxyMatch{match: matched, known: true, adapter: adapter}
	case C.Domain, C.DomainSuffix, C.DomainKeyword, C.DomainRegex, C.DomainWildcard, C.GEOSITE, C.MATCH,
		C.SrcIPCIDR, C.SrcGEOIP, C.SrcIPASN, C.SrcIPSuffix, C.SrcPort, C.InPort, C.Network,
		C.InName, C.InType, C.InUser, C.DSCP, C.ProcessName, C.ProcessPath, C.ProcessNameRegex,
		C.ProcessPathRegex, C.ProcessNameWildcard, C.ProcessPathWildcard, C.Uid, C.RematchName:
		n.kind = dnsNodeLeaf
	case C.AND, C.OR, C.NOT:
		if children, ok := rule.(dnsProxyRuleChildren); ok {
			switch rule.RuleType() {
			case C.AND:
				n.kind = dnsNodeAnd
			case C.OR:
				n.kind = dnsNodeOr
			case C.NOT:
				n.kind = dnsNodeNot
			}
			for _, child := range children.Rules() {
				n.children = append(n.children, compileDNSNode(child, depth+1))
			}
		}
	}
	return n
}

func (n *dnsCompiledNode) eval(md *C.Metadata, e dnsRuleContext, depth int) dnsProxyMatch {
	if depth >= dnsProxyMaxDepth {
		return dnsProxyMatch{}
	}
	if e.swapped || md.DstPort != 53 {
		return matchDNSProxyRuleContext(n.rule, md, e, depth)
	}
	switch n.kind {
	case dnsNodeConstant:
		return n.value
	case dnsNodeLeaf:
		matched, adapter := n.rule.Match(md, e.helper)
		return dnsProxyMatch{match: matched, known: true, adapter: adapter}
	case dnsNodeWrapper:
		w := n.rule.(C.RuleWrapper)
		if w.IsDisabled() {
			return dnsProxyMatch{known: true}
		}
		result := n.children[0].eval(md, e, depth+1)
		if count, ok := w.(interface {
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
	case dnsNodeNot:
		if len(n.children) != 1 {
			return dnsProxyMatch{}
		}
		r := n.children[0].eval(md, e, depth+1)
		if r.known {
			r.match = !r.match
		}
		r.adapter = n.rule.Adapter()
		return r
	case dnsNodeAnd, dnsNodeOr:
		unknown := false
		for _, child := range n.children {
			r := child.eval(md, e, depth+1)
			if !r.known {
				unknown = true
				continue
			}
			if n.kind == dnsNodeOr && r.match {
				return dnsProxyMatch{match: true, known: true, adapter: n.rule.Adapter()}
			}
			if n.kind == dnsNodeAnd && !r.match {
				return dnsProxyMatch{known: true, adapter: n.rule.Adapter()}
			}
		}
		return dnsProxyMatch{match: n.kind == dnsNodeAnd && !unknown, known: !unknown, adapter: n.rule.Adapter()}
	}
	return matchDNSProxyRuleContext(n.rule, md, e, depth)
}

func prepareDNSCompiled(rules []C.Rule, sub map[string][]C.Rule) *dnsCompiledSnapshot {
	snapshot := &dnsCompiledSnapshot{nodes: make(map[C.Rule]*dnsCompiledNode)}
	add := func(rule C.Rule) {
		if rule != nil && reflect.TypeOf(rule).Comparable() {
			snapshot.nodes[rule] = compileDNSNode(rule, 0)
		}
	}
	for _, r := range rules {
		add(r)
	}
	for _, list := range sub {
		for _, r := range list {
			add(r)
		}
	}
	return snapshot
}

func evaluateDNSCompiled(rule C.Rule, md *C.Metadata, e dnsRuleContext) dnsProxyMatch {
	if snapshot := dnsCompiled.Load(); snapshot != nil && reflect.TypeOf(rule).Comparable() {
		if node := snapshot.nodes[rule]; node != nil {
			return node.eval(md, e, 0)
		}
	}
	return matchDNSProxyRuleContext(rule, md, e, 0)
}

// Classical providers are immutable strategy snapshots after loading. Index
// contiguous pure-domain runs only. PROCESS, SRC, IN, wrappers and all logical
// nodes stay in their original order, including their lookup/counter effects.
type dnsClassicalSegment struct {
	domains *trie.DomainSet
	nodes   []*dnsCompiledNode
}
type dnsClassicalView struct{ segments []dnsClassicalSegment }
type dnsClassicalEntry struct {
	once sync.Once
	view dnsClassicalView
}

var dnsClassicalViews = struct {
	sync.RWMutex
	entries map[any]*dnsClassicalEntry
}{entries: make(map[any]*dnsClassicalEntry)}

func lowerASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 128 || (s[i] >= 'A' && s[i] <= 'Z') {
			return false
		}
	}
	return true
}
func dnsIndexPattern(rule C.Rule) (string, bool) {
	if _, wrapped := rule.(C.RuleWrapper); wrapped {
		return "", false
	}
	kind := rule.RuleType()
	if kind != C.Domain && kind != C.DomainSuffix {
		return "", false
	}
	name := rule.Payload()
	if name == "" || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.ContainsAny(name, "*+/\\") || !lowerASCII(name) {
		return "", false
	}
	if kind == C.DomainSuffix {
		name = "+." + name
	}
	return name, true
}
func buildDNSClassical(list []C.Rule) dnsClassicalView {
	var view dnsClassicalView
	for i := 0; i < len(list); {
		start := i
		var builder trie.DomainSetBuilder
		for i < len(list) {
			pattern, ok := dnsIndexPattern(list[i])
			if !ok {
				break
			}
			if builder.Insert(pattern) != nil {
				break
			}
			i++
		}
		if i-start >= 4 {
			view.segments = append(view.segments, dnsClassicalSegment{domains: builder.Build()})
		} else {
			for j := start; j < i; j++ {
				view.segments = append(view.segments, dnsClassicalSegment{nodes: []*dnsCompiledNode{compileDNSNode(list[j], 0)}})
			}
		}
		if i < len(list) {
			view.segments = append(view.segments, dnsClassicalSegment{nodes: []*dnsCompiledNode{compileDNSNode(list[i], 0)}})
			i++
		}
	}
	return view
}
func matchDNSClassical(strategy any, md *C.Metadata, e dnsRuleContext, depth int) dnsProxyMatch {
	children, ok := strategy.(dnsProxyRuleChildren)
	if !ok {
		return dnsProxyMatch{}
	}
	if e.swapped || md.DstPort != 53 || !lowerASCII(md.RuleHost()) || !reflect.TypeOf(strategy).Comparable() {
		return matchDNSProxyChildrenContext(children.Rules(), C.OR, md, e, depth)
	}
	dnsClassicalViews.RLock()
	entry := dnsClassicalViews.entries[strategy]
	dnsClassicalViews.RUnlock()
	if entry == nil {
		dnsClassicalViews.Lock()
		entry = dnsClassicalViews.entries[strategy]
		if entry == nil {
			if len(dnsClassicalViews.entries) >= 64 {
				for key := range dnsClassicalViews.entries {
					delete(dnsClassicalViews.entries, key)
					break
				}
			}
			entry = &dnsClassicalEntry{}
			dnsClassicalViews.entries[strategy] = entry
		}
		dnsClassicalViews.Unlock()
	}
	entry.once.Do(func() { entry.view = buildDNSClassical(children.Rules()) })
	unknown := false
	for _, segment := range entry.view.segments {
		if segment.domains != nil {
			if segment.domains.Has(md.RuleHost()) {
				return dnsProxyMatch{match: true, known: true}
			}
			continue
		}
		for _, node := range segment.nodes {
			r := node.eval(md, e, depth)
			if !r.known {
				unknown = true
			} else if r.match {
				return dnsProxyMatch{match: true, known: true}
			}
		}
	}
	return dnsProxyMatch{known: !unknown}
}

//go:build linux

package transparentproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/nftables"
	"github.com/metacubex/nftables/userdata"
	"golang.org/x/sys/unix"
)

type nftFirewall struct {
	mu    sync.Mutex
	read  func(context.Context) (*nftSnapshot, error)
	write func(context.Context, uint32, []wireMessage) error
}

func newFirewall() (firewall, error) {
	return &nftFirewall{read: readNFTSnapshot, write: writeNFTBatch}, nil
}

func (f *nftFirewall) Apply(ctx context.Context, state firewallState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !state.Enabled {
		return f.close(ctx)
	}
	desired, err := buildNFTRuleset(state)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		actual, err := f.read(ctx)
		if err != nil {
			return fmt.Errorf("read transparent-proxy nftables: %w", err)
		}
		plan, err := planNFTChanges(actual, desired)
		if err != nil {
			return err
		}
		err = f.commit(ctx, plan)
		if errors.Is(err, unix.EAGAIN) && attempt == 0 {
			// Retry one concurrent administrative change immediately. A busy
			// ruleset is handed back to the manager's bounded error backoff.
			continue
		}
		return err
	}
}

func (f *nftFirewall) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.close(ctx)
}

func (f *nftFirewall) close(ctx context.Context) error {
	for attempt := 0; ; attempt++ {
		actual, err := f.read(ctx)
		if err != nil {
			return fmt.Errorf("read transparent-proxy nftables for removal: %w", err)
		}
		if actual.table == nil {
			return nil
		}
		if !hasNFTModuleObjects(actual) {
			return nil
		}
		plan := planNFTClose(actual)
		err = f.commit(ctx, plan)
		if errors.Is(err, unix.EAGAIN) && attempt == 0 {
			continue
		}
		if errors.Is(err, unix.EBUSY) {
			// The complete transaction was rejected without changing anything.
			// Remove capture first, then retain only objects needed by external
			// rules. Kernel reference counts cover unfamiliar expression types.
			return f.closeReferenced(ctx)
		}
		return err
	}
}

func planNFTClose(actual *nftSnapshot) *nftChangePlan {
	plan := &nftChangePlan{generation: actual.generation}
	if actual.comment == nftModule {
		plan.deleteTable = actual.table
	}
	for name, chain := range actual.chains {
		if !managedNFTChain(name) {
			continue
		}
		for _, rule := range chain.rules {
			if managedNFTRule(rule, name) {
				plan.deleteRules = append(plan.deleteRules, rule.rule)
			}
		}
		if actual.comment == nftModule {
			plan.deleteChains = append(plan.deleteChains, chain.chain)
		}
	}
	for name, set := range actual.sets {
		if set.set.Comment == nftModule+"/set/"+name && managedNFTSet(name) {
			plan.deleteSets = append(plan.deleteSets, set.set)
		}
	}
	return plan
}

func (f *nftFirewall) closeReferenced(ctx context.Context) error {
	var targets *nftChangePlan
	for attempt := 0; ; attempt++ {
		actual, err := f.read(ctx)
		if err != nil {
			return err
		}
		if actual.table == nil {
			return nil
		}
		if !hasNFTModuleObjects(actual) {
			return nil
		}
		targets = planNFTClose(actual)
		err = f.commit(ctx, &nftChangePlan{generation: actual.generation, deleteRules: targets.deleteRules})
		if errors.Is(err, unix.EAGAIN) && attempt == 0 {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	// NONREC protects foreign rules in known chains; deleting a set always
	// checks its live references. A table is only deleted when truly empty.
	for _, chain := range targets.deleteChains {
		if err := f.closeContainer(ctx, chain.Name, false); err != nil {
			return err
		}
	}
	for _, set := range targets.deleteSets {
		if err := f.closeContainer(ctx, set.Name, true); err != nil {
			return err
		}
	}
	return f.closeContainer(ctx, "", false)
}

func (f *nftFirewall) closeContainer(ctx context.Context, name string, set bool) error {
	for attempt := 0; ; attempt++ {
		actual, err := f.read(ctx)
		if err != nil {
			return err
		}
		if actual.table == nil {
			return nil
		}
		if !hasNFTModuleObjects(actual) {
			return nil
		}
		plan := &nftChangePlan{generation: actual.generation}
		if name == "" {
			if len(planNFTClose(actual).deleteRules) != 0 {
				return errors.New("managed nftables capture changed while stopping")
			}
			if actual.comment != nftModule {
				return nil
			}
			plan.deleteTable = actual.table
		} else if set {
			present := actual.sets[name]
			if present == nil || present.set.Comment != nftModule+"/set/"+name {
				return nil
			}
			plan.deleteSets = append(plan.deleteSets, present.set)
		} else if present := actual.chains[name]; present != nil && actual.comment == nftModule {
			plan.deleteChains = append(plan.deleteChains, present.chain)
		}
		err = f.commit(ctx, plan)
		if errors.Is(err, unix.EAGAIN) && attempt == 0 {
			continue
		}
		if errors.Is(err, unix.EBUSY) {
			if name == "" {
				log.Warnln("[Transparent Proxy] Managed capture removed; keeping nftables table %s and containers required by external objects", nftTableName)
			}
			return nil
		}
		return err
	}
}

type nftChangePlan struct {
	generation   uint32
	createTable  *nftables.Table
	deleteTable  *nftables.Table
	deleteRules  []*nftables.Rule
	deleteChains []*nftables.Chain
	deleteSets   []*nftables.Set
	createSets   []*nftDesiredSet
	updateSets   []*nftDesiredSet
	createChains []*nftables.Chain
	createRules  []*nftables.Rule
}

func (plan *nftChangePlan) empty() bool {
	return plan.createTable == nil && plan.deleteTable == nil && len(plan.deleteRules)+len(plan.deleteChains)+len(plan.deleteSets)+len(plan.createSets)+len(plan.updateSets)+len(plan.createChains)+len(plan.createRules) == 0
}

func planNFTChanges(actual *nftSnapshot, desired *nftRuleset) (*nftChangePlan, error) {
	plan := &nftChangePlan{generation: actual.generation}
	if actual.table != nil {
		if err := requireNFTModule(actual); err != nil {
			return nil, err
		}
		if actual.table.Flags != 0 {
			return nil, errors.New("transparent-proxy nftables table flags were changed externally")
		}
	} else {
		plan.createTable = desired.table
	}
	// Changes to chain hooks or set key types require removing their own
	// references in the same transaction. Ordinary address changes only
	// replace set elements; unrelated rules keep their handles and order.
	rebuildRules := false
	for _, wanted := range desired.sets {
		present := actual.sets[wanted.set.Name]
		if present == nil {
			plan.createSets = append(plan.createSets, wanted)
			continue
		}
		if present.set.Comment != wanted.set.Comment {
			return nil, fmt.Errorf("transparent-proxy nftables set %q has another owner", wanted.set.Name)
		}
		if !equalNFTSetShape(present, wanted.set) {
			plan.deleteSets = append(plan.deleteSets, present.set)
			plan.createSets = append(plan.createSets, wanted)
			rebuildRules = true
		} else if !equalNFTElements(present.elements, wanted.elements) {
			plan.updateSets = append(plan.updateSets, wanted)
		}
	}
	for _, wanted := range desired.chains {
		present := actual.chains[wanted.chain.Name]
		if present == nil {
			plan.createChains = append(plan.createChains, wanted.chain)
			continue
		}
		for _, rule := range present.rules {
			if !managedNFTRule(rule, wanted.chain.Name) {
				return nil, fmt.Errorf("transparent-proxy nftables chain %q contains a foreign rule", wanted.chain.Name)
			}
		}
		if !equalNFTChain(present.chain, wanted.chain) {
			plan.deleteChains = append(plan.deleteChains, present.chain)
			plan.createChains = append(plan.createChains, wanted.chain)
			rebuildRules = true
		}
	}
	for _, wanted := range desired.chains {
		present := actual.chains[wanted.chain.Name]
		if !rebuildRules && present != nil {
			equal, err := equalNFTRules(present.rules, wanted.rules)
			if err != nil {
				return nil, err
			}
			if equal {
				continue
			}
		}
		if present != nil {
			for _, rule := range present.rules {
				plan.deleteRules = append(plan.deleteRules, rule.rule)
			}
		}
		plan.createRules = append(plan.createRules, wanted.rules...)
	}
	return plan, nil
}

func requireNFTModule(actual *nftSnapshot) error {
	if actual.table != nil && actual.comment == nftModule {
		return nil
	}
	return fmt.Errorf("nftables table %q has no mihomo-optimized module marker", nftTableName)
}

func hasNFTModuleObjects(actual *nftSnapshot) bool {
	if actual.table == nil {
		return false
	}
	if actual.comment == nftModule {
		return true
	}
	for name, chain := range actual.chains {
		if managedNFTChain(name) {
			for _, rule := range chain.rules {
				if managedNFTRule(rule, name) {
					return true
				}
			}
		}
	}
	for name, set := range actual.sets {
		if managedNFTSet(name) && set.set.Comment == nftModule+"/set/"+name {
			return true
		}
	}
	return false
}

func managedNFTRule(rule *nftRuleState, chain string) bool {
	return strings.HasPrefix(rule.comment, nftModule+"/rule/"+chain+"/")
}

func managedNFTChain(name string) bool {
	switch name {
	case "prerouting", "output", "pre4", "pre6", "out4", "out6", "tproxy4", "tproxy6", "mark_tproxy4", "mark_tproxy6", "tun4", "tun6":
		return true
	case "redir4", "redir6", "redir_nat_pre", "redir_nat_out", "redir_guard_pre", "redir_guard_out", "redir_check4", "redir_check6":
		return true
	}
	return false
}

func managedNFTSet(name string) bool {
	switch name {
	case "local4", "local6", "gateway", "bypass_uids", "bypass_marks", "bypass_udp_ports", "bypass_tcp_ports", "local_types", "fake4", "fake6", "bypass4", "bypass6":
		return true
	}
	return false
}

func equalNFTSetShape(actual *nftSetState, desired *nftables.Set) bool {
	flags := uint32(0)
	if desired.Interval {
		flags = unix.NFT_SET_INTERVAL
	}
	return actual.flags == flags && actual.set.KeyType.GetNFTMagic() == desired.KeyType.GetNFTMagic() && actual.set.KeyType.Bytes == desired.KeyType.Bytes
}

func equalNFTElements(actual, desired []nftables.SetElement) bool {
	if len(actual) != len(desired) {
		return false
	}
	for i, element := range actual {
		if !bytes.Equal(element.Key, desired[i].Key) || element.IntervalEnd != desired[i].IntervalEnd || element.Comment != desired[i].Comment {
			return false
		}
	}
	return true
}

func equalNFTChain(actual, desired *nftables.Chain) bool {
	return actual.Name == desired.Name && actual.Type == desired.Type && actual.Device == desired.Device &&
		reflect.DeepEqual(actual.Hooknum, desired.Hooknum) && reflect.DeepEqual(actual.Priority, desired.Priority) && reflect.DeepEqual(actual.Policy, desired.Policy)
}

func equalNFTRules(actual []*nftRuleState, desired []*nftables.Rule) (bool, error) {
	if len(actual) != len(desired) {
		return false, nil
	}
	for i, rule := range desired {
		fingerprint, err := nftRuleFingerprint(rule)
		if err != nil {
			return false, err
		}
		comment, err := nftUserdataString(rule.UserData, byte(userdata.TypeComment))
		if err != nil {
			return false, err
		}
		if actual[i].comment != comment || !bytes.Equal(actual[i].expressions, fingerprint) {
			return false, nil
		}
	}
	return true, nil
}

func (f *nftFirewall) commit(ctx context.Context, plan *nftChangePlan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan.empty() {
		return nil
	}
	operations, err := encodeNFTChanges(plan)
	if err != nil {
		return err
	}
	if err := f.write(ctx, plan.generation, operations); err != nil {
		return fmt.Errorf("apply transparent-proxy nftables transaction: %w", err)
	}
	return nil
}

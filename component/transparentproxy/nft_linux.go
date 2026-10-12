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

	"github.com/metacubex/nftables"
	"github.com/metacubex/nftables/userdata"
	"golang.org/x/sys/unix"
)

type nftFirewall struct {
	mu    sync.Mutex
	owner string
	read  func(context.Context, string) (*nftSnapshot, error)
	write func(context.Context, uint32, []wireMessage) error
}

func newFirewall(owner string) (firewall, error) {
	if _, err := buildNFTRuleset(owner, firewallState{TCP: ModeOff, UDP: ModeOff}); err != nil {
		return nil, err
	}
	return &nftFirewall{owner: owner, read: readNFTSnapshot, write: writeNFTBatch}, nil
}

func (f *nftFirewall) Apply(ctx context.Context, state firewallState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !state.Enabled {
		return f.close(ctx)
	}
	desired, err := buildNFTRuleset(f.owner, state)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		actual, err := f.read(ctx, f.owner)
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
	owner := nftOwner(f.owner)
	for attempt := 0; ; attempt++ {
		actual, err := f.read(ctx, f.owner)
		if err != nil {
			return fmt.Errorf("read transparent-proxy nftables for removal: %w", err)
		}
		if actual.table == nil {
			return nil
		}
		if err := requireNFTOwner(actual, owner); err != nil {
			return err
		}
		plan := &nftChangePlan{generation: actual.generation}
		for _, chain := range actual.chains {
			for _, rule := range chain.rules {
				if strings.HasPrefix(rule.comment, owner+"/rule/") {
					plan.deleteRules = append(plan.deleteRules, rule.rule)
				}
			}
		}
		// Keep the ownership marker/table. It permits safe recovery after
		// reexec, and avoids deleting foreign objects that an administrator
		// may have added. Empty chains/sets are bounded and have no capture.
		err = f.commit(ctx, plan)
		if errors.Is(err, unix.EAGAIN) && attempt == 0 {
			continue
		}
		return err
	}
}

type nftChangePlan struct {
	generation   uint32
	createTable  *nftables.Table
	deleteRules  []*nftables.Rule
	deleteChains []*nftables.Chain
	deleteSets   []*nftables.Set
	createSets   []*nftDesiredSet
	updateSets   []*nftDesiredSet
	createChains []*nftables.Chain
	createRules  []*nftables.Rule
}

func (plan *nftChangePlan) empty() bool {
	return plan.createTable == nil && len(plan.deleteRules)+len(plan.deleteChains)+len(plan.deleteSets)+len(plan.createSets)+len(plan.updateSets)+len(plan.createChains)+len(plan.createRules) == 0
}

func planNFTChanges(actual *nftSnapshot, desired *nftRuleset) (*nftChangePlan, error) {
	plan := &nftChangePlan{generation: actual.generation}
	if actual.table != nil {
		if err := requireNFTOwner(actual, desired.owner); err != nil {
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
			if !strings.HasPrefix(rule.comment, desired.owner+"/rule/"+wanted.chain.Name+"/") {
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

func requireNFTOwner(actual *nftSnapshot, owner string) error {
	// Any intact owner-tagged object proves the same persistent installation.
	// This also recovers when an administrator flushed/deleted the dedicated
	// marker while leaving other owned objects. An empty, unknown table is
	// deliberately not adopted merely because it has our usual name.
	for _, set := range actual.sets {
		if set.set.Comment == owner+"/set/"+set.set.Name {
			return nil
		}
		if set.set.Name == "owner" {
			for _, element := range set.elements {
				if element.Comment == owner && !element.IntervalEnd {
					return nil
				}
			}
		}
	}
	for _, chain := range actual.chains {
		for _, rule := range chain.rules {
			if strings.HasPrefix(rule.comment, owner+"/rule/") {
				return nil
			}
		}
	}
	return fmt.Errorf("nftables table %q is not owned by this mihomo-optimized installation", nftTableName)
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

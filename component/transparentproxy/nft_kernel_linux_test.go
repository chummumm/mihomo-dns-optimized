//go:build linux

package transparentproxy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/metacubex/nftables"
	"github.com/metacubex/nftables/expr"
	"github.com/metacubex/nftables/userdata"
)

func TestNFTKernelRoundTrip(t *testing.T) {
	if os.Getenv("MIHOMO_TRANSPARENT_PROXY_INTEGRATION") != "1" {
		t.Skip("requires explicit privileged integration run in an isolated network namespace")
	}
	parentNamespace := os.Getenv("MIHOMO_NFT_PARENT_NAMESPACE")
	currentNamespace, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	if parentNamespace == "" {
		// No network mutation occurs in the parent test process. Required CI
		// runs fail if unshare is unavailable or lacks namespace capabilities.
		command := exec.Command("unshare", "--net", os.Args[0], "-test.run", "^TestNFTKernelRoundTrip$", "-test.v")
		command.Env = append(os.Environ(), "MIHOMO_NFT_PARENT_NAMESPACE="+currentNamespace)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated nftables integration: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}
	if parentNamespace == currentNamespace {
		t.Fatal("refusing nftables integration in the parent network namespace")
	}

	ctx := context.Background()
	instance, err := newFirewall("kernel-test-owner")
	if err != nil {
		t.Fatal(err)
	}
	f := instance.(*nftFirewall)
	state := nftTestState()
	if err := f.Apply(ctx, state); err != nil {
		t.Fatal(err)
	}
	assertConverged := func() *nftSnapshot {
		t.Helper()
		actual, err := readNFTSnapshot(ctx, f.owner)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := buildNFTRuleset(f.owner, state)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := planNFTChanges(actual, desired)
		if err != nil {
			t.Fatal(err)
		}
		if !plan.empty() {
			for _, chain := range desired.chains {
				present := actual.chains[chain.chain.Name]
				if present == nil {
					t.Logf("missing chain: %s", chain.chain.Name)
					continue
				}
				if equal, _ := equalNFTRules(present.rules, chain.rules); !equal {
					t.Logf("different chain: %s actual=%d desired=%d", chain.chain.Name, len(present.rules), len(chain.rules))
					for i := 0; i < len(present.rules) && i < len(chain.rules); i++ {
						wanted, _ := nftRuleFingerprint(chain.rules[i])
						if !bytes.Equal(wanted, present.rules[i].expressions) {
							t.Logf("rule %d actual=%x desired=%x", i, present.rules[i].expressions, wanted)
							break
						}
					}
				}
			}
			t.Fatalf("kernel snapshot did not converge: addRules=%d deleteRules=%d addSets=%d updateSets=%d addChains=%d", len(plan.createRules), len(plan.deleteRules), len(plan.createSets), len(plan.updateSets), len(plan.createChains))
		}
		return actual
	}
	actual := assertConverged()
	write := f.write
	f.write = func(context.Context, uint32, []wireMessage) error {
		return fmt.Errorf("unchanged Apply must not send a mutation transaction")
	}
	if err := f.Apply(ctx, state); err != nil {
		t.Fatal(err)
	}
	f.write = write

	admin, err := nftables.New(nftables.AsLasting())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.CloseLasting()
	if err := admin.DelRule(actual.chains["pre4"].rules[0].rule); err != nil {
		t.Fatal(err)
	}
	if err := admin.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Apply(ctx, state); err != nil {
		t.Fatal(err)
	}
	actual = assertConverged()
	admin.FlushSet(actual.sets["fake4"].set)
	if err := admin.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Apply(ctx, state); err != nil {
		t.Fatal(err)
	}
	assertConverged()
	admin.DelTable(actual.table)
	if err := admin.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Apply(ctx, state); err != nil {
		t.Fatal(err)
	}
	actual = assertConverged()

	for _, tcp := range []Mode{ModeOff, ModeTProxy, ModeTUN} {
		for _, udp := range []Mode{ModeOff, ModeTProxy, ModeTUN} {
			state.TCP, state.UDP = tcp, udp
			if err := f.Apply(ctx, state); err != nil {
				t.Fatalf("%s/%s: %v", tcp, udp, err)
			}
			assertConverged()
		}
	}
	state = nftTestState()
	state.IPv6, state.Tun4, state.TProxyTCP4 = false, false, false
	if err := f.Apply(ctx, state); err != nil {
		t.Fatal(err)
	}
	assertConverged()
	state = nftTestState()
	if err := f.Apply(ctx, state); err != nil {
		t.Fatal(err)
	}
	actual = assertConverged()

	foreignTable := admin.AddTable(&nftables.Table{Name: "transparent_proxy_foreign", Family: nftables.TableFamilyINet})
	foreignChain := admin.AddChain(&nftables.Chain{Name: "administrator", Table: actual.table})
	admin.AddRule(&nftables.Rule{Table: actual.table, Chain: foreignChain, Exprs: []expr.Any{&expr.Counter{}, nftReturn()}, UserData: userdata.AppendString(nil, userdata.TypeComment, "administrator")})
	foreignSet := &nftables.Set{Name: "administrator_addresses", Table: actual.table, KeyType: nftables.TypeIPAddr}
	if err := admin.AddSet(foreignSet, []nftables.SetElement{{Key: []byte{203, 0, 113, 7}}}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Apply(ctx, state); err != nil {
		t.Fatal(err)
	}
	assertConverged()
	if err := f.Close(ctx); err != nil {
		t.Fatal(err)
	}
	afterClose, err := readNFTSnapshot(ctx, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	for name, chain := range afterClose.chains {
		want := 0
		if name == "administrator" {
			want = 1
		}
		if len(chain.rules) != want {
			t.Fatalf("Close left/deleted unexpected rules in %s: %d", name, len(chain.rules))
		}
	}
	if _, err := admin.ListTableOfFamily(foreignTable.Name, foreignTable.Family); err != nil {
		t.Fatalf("external table changed: %v", err)
	}
	elements, err := admin.GetSetElements(foreignSet)
	if err != nil || len(elements) != 1 || !bytes.Equal(elements[0].Key, []byte{203, 0, 113, 7}) {
		t.Fatalf("external set changed: %+v %v", elements, err)
	}
	if err := f.Apply(ctx, state); err != nil {
		t.Fatal(err)
	}
	assertConverged()

	other, err := newFirewall("different-installation")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Apply(ctx, state); err == nil {
		t.Fatal("adopted another installation's table")
	}
	if err := other.Close(ctx); err == nil {
		t.Fatal("removed another installation's table")
	}
	unknown := &nftables.Table{Name: nftTableName, Family: nftables.TableFamilyINet}
	unknownChain := &nftables.Chain{Name: "untouched", Table: unknown}
	state.Tun4 = false
	replaced := false
	f.write = func(ctx context.Context, generation uint32, messages []wireMessage) error {
		if !replaced {
			replaced = true
			// The replacement deliberately happens after our complete owner
			// snapshot. GENID must prevent every operation against this table.
			admin.DelTable(actual.table)
			admin.AddTable(unknown)
			admin.AddChain(unknownChain)
			admin.AddRule(&nftables.Rule{Table: unknown, Chain: unknownChain, Exprs: []expr.Any{nftReturn()}})
			if err := admin.Flush(); err != nil {
				return err
			}
		}
		return write(ctx, generation, messages)
	}
	if err := f.Apply(ctx, state); err == nil {
		t.Fatal("adopted externally replaced same-name table")
	}
	if !replaced {
		t.Fatal("did not exercise snapshot-to-commit table replacement")
	}
	if err := f.Close(ctx); err == nil {
		t.Fatal("removed externally replaced same-name table")
	}
	rules, err := admin.GetRules(unknown, unknownChain)
	if err != nil || len(rules) != 1 {
		t.Fatalf("external replacement was modified: %d %v", len(rules), err)
	}
}

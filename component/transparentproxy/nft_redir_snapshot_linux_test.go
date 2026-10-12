//go:build linux

package transparentproxy

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/metacubex/nftables"
	"github.com/metacubex/nftables/binaryutil"
	"github.com/metacubex/nftables/expr"
	"golang.org/x/sys/unix"
)

func TestNFTKernelRedirDefaultAttributesConverge(t *testing.T) {
	if binaryutil.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("fixtures were captured from a little-endian Linux kernel")
	}
	// Exact expression-list elements read back in CI job 114794278289.
	// Linux adds NFTA_BITWISE_OP=NFT_BITWISE_BOOL and records the specified
	// redirect port with NFTA_REDIR_FLAGS=NF_NAT_RANGE_PROTO_SPECIFIED.
	bitwise := nftRedirKernelExpressionFixture(t, "4c0001000c00010062697477697365003c0002000800010000000001080002000000000108000300000000040c00040008000100200000000c00050008000100000000000800060000000000")
	redir := nftRedirKernelExpressionFixture(t, "2c0001000a00010072656469720000001c000200080001000000000108000200000000010800030000000002")
	state := nftTestState()
	state.TCP = ModeRedir
	program, err := buildNFTRuleset(state)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := nftSnapshotFromProgram(t, program)
	bitwiseCount, redirCount := 0, 0
	for _, name := range []string{"redir_nat_pre", "redir_nat_out", "redir_check4", "redir_check6"} {
		chain := snapshot.chains[name]
		for i, previous := range chain.rules {
			rule := previous.rule
			var expressions []byte
			for _, expression := range rule.Exprs {
				var data []byte
				switch expression.(type) {
				case *expr.Bitwise:
					data = bitwise
					bitwiseCount++
				case *expr.Redir:
					data = redir
					redirCount++
				default:
					data, err = expr.Marshal(byte(nftables.TableFamilyINet), expression)
					if err != nil {
						t.Fatal(err)
					}
				}
				expressions = appendWireAttribute(expressions, unix.NFTA_LIST_ELEM, data)
			}
			data := []byte{unix.NFPROTO_INET, unix.NFNETLINK_V0, 0, 0}
			data = appendWireAttribute(data, unix.NFTA_RULE_TABLE, []byte(program.table.Name+"\x00"))
			data = appendWireAttribute(data, unix.NFTA_RULE_CHAIN, []byte(name+"\x00"))
			data = appendWireAttribute(data, unix.NFTA_RULE_HANDLE, binaryutil.BigEndian.PutUint64(rule.Handle))
			data = appendWireAttribute(data, unix.NFTA_RULE_USERDATA, rule.UserData)
			data = appendWireAttribute(data, unix.NFTA_RULE_EXPRESSIONS, expressions)
			chain.rules[i], err = decodeNFTRule(wireMessage{Type: uint16(unix.NFNL_SUBSYS_NFTABLES<<8) | unix.NFT_MSG_NEWRULE, Data: data})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if bitwiseCount == 0 || redirCount == 0 {
		t.Fatal("kernel fixtures did not exercise both REDIRECT expression types")
	}
	plan, err := planNFTChanges(snapshot, program)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.empty() {
		t.Fatalf("kernel REDIRECT defaults caused a rewrite: addRules=%d deleteRules=%d", len(plan.createRules), len(plan.deleteRules))
	}
}

func nftRedirKernelExpressionFixture(t *testing.T, value string) []byte {
	t.Helper()
	data, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	items, err := parseWireAttributes(data)
	if err != nil || len(items) != 1 || items[0].Type != unix.NFTA_LIST_ELEM {
		t.Fatalf("invalid kernel expression fixture: %v", err)
	}
	return items[0].Data
}

func TestNFTBitwiseNormalizationPreservesSignificantAttributes(t *testing.T) {
	data, err := expr.Marshal(byte(nftables.TableFamilyINet), &expr.Bitwise{
		SourceRegister: 1, DestRegister: 1, Len: 4,
		Mask: binaryutil.NativeEndian.PutUint32(1 << 5), Xor: binaryutil.NativeEndian.PutUint32(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	wanted, err := canonicalNFTExpression(data)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := nftAttributeMap(data)
	if err != nil {
		t.Fatal(err)
	}
	operand := func(value uint32) []byte {
		return appendWireAttribute(nil, unix.NFTA_DATA_VALUE, binaryutil.NativeEndian.PutUint32(value))
	}
	for _, test := range []struct {
		name               string
		kind               uint16
		value              []byte
		remove, equal, bad bool
	}{
		{name: "default boolean operation", kind: nftBitwiseOperation, value: binaryutil.BigEndian.PutUint32(unix.NFT_BITWISE_BOOL), equal: true},
		{name: "left shift", kind: nftBitwiseOperation, value: binaryutil.BigEndian.PutUint32(1)}, // NFT_BITWISE_LSHIFT
		{name: "future operation", kind: nftBitwiseOperation, value: binaryutil.BigEndian.PutUint32(99)},
		{name: "source register", kind: unix.NFTA_BITWISE_SREG, value: binaryutil.BigEndian.PutUint32(2)},
		{name: "destination register", kind: unix.NFTA_BITWISE_DREG, value: binaryutil.BigEndian.PutUint32(2)},
		{name: "operand length", kind: unix.NFTA_BITWISE_LEN, value: binaryutil.BigEndian.PutUint32(8)},
		{name: "zero length", kind: unix.NFTA_BITWISE_LEN, value: binaryutil.BigEndian.PutUint32(0)},
		{name: "mask", kind: unix.NFTA_BITWISE_MASK, value: operand(1 << 6)},
		{name: "xor", kind: unix.NFTA_BITWISE_XOR, value: operand(1)},
		{name: "missing mask", kind: unix.NFTA_BITWISE_MASK, remove: true},
		{name: "missing xor", kind: unix.NFTA_BITWISE_XOR, remove: true},
		{name: "shift data", kind: 7, value: operand(0)}, // NFTA_BITWISE_DATA
		{name: "unknown zero attribute", kind: 0x3ffe, value: binaryutil.BigEndian.PutUint32(0)},
		{name: "extra mask attribute", kind: unix.NFTA_BITWISE_MASK, value: appendWireAttribute(operand(1<<5), 0x3ffe, []byte{0})},
		{name: "empty operation", kind: nftBitwiseOperation, bad: true},
		{name: "short operation", kind: nftBitwiseOperation, value: []byte{0, 0, 0}, bad: true},
		{name: "long operation", kind: nftBitwiseOperation, value: []byte{0, 0, 0, 0, 0}, bad: true},
		{name: "short register", kind: unix.NFTA_BITWISE_SREG, value: []byte{0, 0, 1}, bad: true},
		{name: "malformed mask", kind: unix.NFTA_BITWISE_MASK, value: []byte{1}, bad: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fields, err := nftAttributeMap(outer[unix.NFTA_EXPR_DATA])
			if err != nil {
				t.Fatal(err)
			}
			fields[nftBitwiseOperation] = binaryutil.BigEndian.PutUint32(unix.NFT_BITWISE_BOOL)
			if test.remove {
				delete(fields, test.kind)
			} else {
				fields[test.kind] = test.value
			}
			response := appendWireAttribute(nil, unix.NFTA_EXPR_NAME, outer[unix.NFTA_EXPR_NAME])
			response = appendWireAttribute(response, unix.NFTA_EXPR_DATA, marshalNFTAttributes(fields))
			got, err := canonicalNFTExpression(response)
			if test.bad {
				if !errors.Is(err, errIncompleteNetlink) {
					t.Fatalf("malformed bitwise attributes were not rejected: %v", err)
				}
				return
			}
			if err != nil || bytes.Equal(got, wanted) != test.equal {
				t.Fatalf("significant bitwise change lost or default not normalized: equal=%v want=%v err=%v", bytes.Equal(got, wanted), test.equal, err)
			}
		})
	}
}

func TestNFTRedirFlagsRemainSignificant(t *testing.T) {
	data, err := expr.Marshal(byte(nftables.TableFamilyINet), &expr.Redir{RegisterProtoMin: 1, RegisterProtoMax: 1, Flags: unix.NF_NAT_RANGE_PROTO_SPECIFIED})
	if err != nil {
		t.Fatal(err)
	}
	wanted, err := canonicalNFTExpression(data)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := nftAttributeMap(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, flags := range []uint32{0, unix.NF_NAT_RANGE_PROTO_SPECIFIED | unix.NF_NAT_RANGE_PROTO_RANDOM, unix.NF_NAT_RANGE_PROTO_SPECIFIED | 1<<31} {
		fields, err := nftAttributeMap(outer[unix.NFTA_EXPR_DATA])
		if err != nil {
			t.Fatal(err)
		}
		fields[unix.NFTA_REDIR_FLAGS] = binaryutil.BigEndian.PutUint32(flags)
		response := appendWireAttribute(nil, unix.NFTA_EXPR_NAME, outer[unix.NFTA_EXPR_NAME])
		response = appendWireAttribute(response, unix.NFTA_EXPR_DATA, marshalNFTAttributes(fields))
		got, err := canonicalNFTExpression(response)
		if err != nil || bytes.Equal(got, wanted) {
			t.Fatalf("redirect flags %#x were normalized away: %v", flags, err)
		}
	}
}

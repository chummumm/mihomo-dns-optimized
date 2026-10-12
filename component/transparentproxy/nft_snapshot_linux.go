//go:build linux

package transparentproxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/metacubex/nftables"
	"github.com/metacubex/nftables/expr"
	"github.com/metacubex/nftables/userdata"
	"golang.org/x/sys/unix"
)

type nftRuleState struct {
	rule        *nftables.Rule
	comment     string
	expressions []byte
}

type nftChainState struct {
	chain *nftables.Chain
	rules []*nftRuleState
}

type nftSetState struct {
	set      *nftables.Set
	flags    uint32
	elements []nftables.SetElement
}

type nftSnapshot struct {
	generation uint32
	table      *nftables.Table
	chains     map[string]*nftChainState
	sets       map[string]*nftSetState
}

// The pinned nftables library silently omits tproxy/socket expressions when
// reading rules, and omits set comments. Raw, strictly checked snapshots avoid
// treating partial rule descriptions as equal (or endlessly rewriting them).
// The paired writer uses the same nftables expression types with a generation
// guard; wireConn owns transport completeness for both reads and writes.
func readNFTSnapshot(ctx context.Context, owner string) (*nftSnapshot, error) {
	connection, err := newWireConn(unix.NETLINK_NETFILTER, 0)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	before, err := readNFTGeneration(ctx, connection)
	if err != nil {
		return nil, err
	}
	snapshot := &nftSnapshot{generation: before, chains: make(map[string]*nftChainState), sets: make(map[string]*nftSetState)}
	request := appendWireAttribute(nil, unix.NFTA_TABLE_NAME, []byte(nftTableName+"\x00"))
	messages, err := nftQuery(ctx, connection, unix.NFT_MSG_GETTABLE, 0, request)
	if errors.Is(err, unix.ENOENT) {
		return snapshot, nil
	}
	if err != nil {
		return nil, err
	}
	if len(messages) != 1 {
		return nil, errors.New("nftables table query did not return one table")
	}
	attributes, err := nftMessageAttributes(messages[0], unix.NFT_MSG_NEWTABLE)
	if err != nil {
		return nil, err
	}
	name, err := nftStringAttribute(attributes, unix.NFTA_TABLE_NAME)
	if err != nil || name != nftTableName {
		return nil, errors.New("nftables returned an unexpected table")
	}
	flags, err := nftUint32Attribute(attributes, unix.NFTA_TABLE_FLAGS, true)
	if err != nil {
		return nil, err
	}
	snapshot.table = &nftables.Table{Name: name, Family: nftables.TableFamilyINet, Flags: flags}

	request = appendWireAttribute(nil, unix.NFTA_CHAIN_TABLE, []byte(nftTableName+"\x00"))
	messages, err = nftQuery(ctx, connection, unix.NFT_MSG_GETCHAIN, unix.NLM_F_DUMP, request)
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		chain, err := decodeNFTChain(message)
		if err != nil {
			return nil, err
		}
		if chain.chain.Table.Name != nftTableName {
			continue
		}
		if _, duplicate := snapshot.chains[chain.chain.Name]; duplicate {
			return nil, errors.New("nftables returned a duplicate chain")
		}
		snapshot.chains[chain.chain.Name] = chain
	}
	request = appendWireAttribute(nil, unix.NFTA_SET_TABLE, []byte(nftTableName+"\x00"))
	messages, err = nftQuery(ctx, connection, unix.NFT_MSG_GETSET, unix.NLM_F_DUMP, request)
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		set, err := decodeNFTSet(message)
		if err != nil {
			return nil, err
		}
		if set.set.Table.Name != nftTableName {
			continue
		}
		if _, duplicate := snapshot.sets[set.set.Name]; duplicate {
			return nil, errors.New("nftables returned a duplicate set")
		}
		snapshot.sets[set.set.Name] = set
		if set.set.Name != "owner" && !strings.HasPrefix(set.set.Comment, nftOwner(owner)+"/set/") {
			continue
		}
		request := appendWireAttribute(nil, unix.NFTA_SET_ELEM_LIST_TABLE, []byte(nftTableName+"\x00"))
		request = appendWireAttribute(request, unix.NFTA_SET_ELEM_LIST_SET, []byte(set.set.Name+"\x00"))
		elementMessages, err := nftQuery(ctx, connection, unix.NFT_MSG_GETSETELEM, unix.NLM_F_DUMP, request)
		if err != nil {
			return nil, err
		}
		for _, elementMessage := range elementMessages {
			elements, err := decodeNFTElements(elementMessage, set.set.Name)
			if err != nil {
				return nil, err
			}
			set.elements = append(set.elements, elements...)
		}
		set.elements = normalizeNFTElements(set.elements)
	}
	request = appendWireAttribute(nil, unix.NFTA_RULE_TABLE, []byte(nftTableName+"\x00"))
	messages, err = nftQuery(ctx, connection, unix.NFT_MSG_GETRULE, unix.NLM_F_DUMP, request)
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		rule, err := decodeNFTRule(message)
		if err != nil {
			return nil, err
		}
		if rule.rule.Table.Name != nftTableName {
			continue
		}
		chain := snapshot.chains[rule.rule.Chain.Name]
		if chain == nil {
			return nil, fmt.Errorf("%w: nftables rule refers to an absent chain", errIncompleteNetlink)
		}
		chain.rules = append(chain.rules, rule)
	}
	after, err := readNFTGeneration(ctx, connection)
	if err != nil {
		return nil, err
	}
	if before != after {
		return nil, fmt.Errorf("%w: nftables changed during snapshot", errIncompleteNetlink)
	}
	return snapshot, nil
}

func nftQuery(ctx context.Context, connection *wireConn, kind uint16, flags uint16, attributes []byte) ([]wireMessage, error) {
	payload := append([]byte{unix.NFPROTO_INET, unix.NFNETLINK_V0, 0, 0}, attributes...)
	return connection.Execute(ctx, uint16(unix.NFNL_SUBSYS_NFTABLES<<8)|kind, flags, payload)
}

func readNFTGeneration(ctx context.Context, connection *wireConn) (uint32, error) {
	messages, err := connection.Execute(ctx, uint16(unix.NFNL_SUBSYS_NFTABLES<<8)|unix.NFT_MSG_GETGEN, 0, []byte{unix.NFPROTO_UNSPEC, unix.NFNETLINK_V0, 0, 0})
	if err != nil {
		return 0, err
	}
	if len(messages) != 1 || messages[0].Type != uint16(unix.NFNL_SUBSYS_NFTABLES<<8)|unix.NFT_MSG_NEWGEN || len(messages[0].Data) < 4 {
		return 0, errors.New("invalid nftables generation response")
	}
	attributes, err := nftAttributeMap(messages[0].Data[4:])
	if err != nil {
		return 0, err
	}
	generation, err := nftUint32Attribute(attributes, unix.NFTA_GEN_ID, false)
	if err == nil && generation == 0 {
		return 0, fmt.Errorf("%w: nftables returned an unguarded zero generation", errIncompleteNetlink)
	}
	return generation, err
}

func nftMessageAttributes(message wireMessage, kind uint16) (map[uint16][]byte, error) {
	if message.Type != uint16(unix.NFNL_SUBSYS_NFTABLES<<8)|kind || len(message.Data) < 4 || message.Data[0] != unix.NFPROTO_INET || message.Data[1] != unix.NFNETLINK_V0 {
		return nil, fmt.Errorf("%w: unexpected nftables message", errIncompleteNetlink)
	}
	return nftAttributeMap(message.Data[4:])
}

func nftAttributeMap(data []byte) (map[uint16][]byte, error) {
	attributes, err := parseWireAttributes(data)
	if err != nil {
		return nil, err
	}
	result := make(map[uint16][]byte, len(attributes))
	for _, attribute := range attributes {
		kind := attribute.Type & 0x3fff
		if previous, duplicate := result[kind]; duplicate {
			// Kernel dumps may insert the same empty PAD attribute before
			// more than one 64-bit field. It carries no state or ownership.
			if len(previous) == 0 && len(attribute.Data) == 0 {
				continue
			}
			return nil, fmt.Errorf("%w: duplicate nftables attribute", errIncompleteNetlink)
		}
		result[kind] = attribute.Data
	}
	return result, nil
}

func nftStringAttribute(attributes map[uint16][]byte, kind uint16) (string, error) {
	value := attributes[kind]
	if len(value) == 0 || value[len(value)-1] != 0 || bytes.IndexByte(value[:len(value)-1], 0) >= 0 {
		return "", fmt.Errorf("%w: invalid nftables string attribute", errIncompleteNetlink)
	}
	return string(value[:len(value)-1]), nil
}

func nftUint32Attribute(attributes map[uint16][]byte, kind uint16, optional bool) (uint32, error) {
	value, present := attributes[kind]
	if !present && optional {
		return 0, nil
	}
	if len(value) != 4 {
		return 0, fmt.Errorf("%w: invalid nftables integer attribute", errIncompleteNetlink)
	}
	return binary.BigEndian.Uint32(value), nil
}

func decodeNFTChain(message wireMessage) (*nftChainState, error) {
	attributes, err := nftMessageAttributes(message, unix.NFT_MSG_NEWCHAIN)
	if err != nil {
		return nil, err
	}
	name, err := nftStringAttribute(attributes, unix.NFTA_CHAIN_NAME)
	if err != nil {
		return nil, err
	}
	table, err := nftStringAttribute(attributes, unix.NFTA_CHAIN_TABLE)
	if err != nil {
		return nil, err
	}
	chain := &nftables.Chain{Name: name, Table: &nftables.Table{Name: table, Family: nftables.TableFamilyINet}}
	if _, present := attributes[unix.NFTA_CHAIN_TYPE]; present {
		kind, err := nftStringAttribute(attributes, unix.NFTA_CHAIN_TYPE)
		if err != nil {
			return nil, err
		}
		chain.Type = nftables.ChainType(kind)
	}
	if _, present := attributes[unix.NFTA_CHAIN_POLICY]; present {
		policy, err := nftUint32Attribute(attributes, unix.NFTA_CHAIN_POLICY, false)
		if err != nil {
			return nil, err
		}
		value := nftables.ChainPolicy(policy)
		chain.Policy = &value
	}
	if data, present := attributes[unix.NFTA_CHAIN_HOOK]; present {
		hook, err := nftAttributeMap(data)
		if err != nil {
			return nil, err
		}
		number, err := nftUint32Attribute(hook, unix.NFTA_HOOK_HOOKNUM, false)
		if err != nil {
			return nil, err
		}
		priority, err := nftUint32Attribute(hook, unix.NFTA_HOOK_PRIORITY, false)
		if err != nil {
			return nil, err
		}
		chain.Hooknum, chain.Priority = nftables.ChainHookRef(nftables.ChainHook(number)), nftables.ChainPriorityRef(nftables.ChainPriority(int32(priority)))
		if _, present := hook[unix.NFTA_HOOK_DEV]; present {
			chain.Device, err = nftStringAttribute(hook, unix.NFTA_HOOK_DEV)
			if err != nil {
				return nil, err
			}
		}
	}
	return &nftChainState{chain: chain}, nil
}

func decodeNFTSet(message wireMessage) (*nftSetState, error) {
	attributes, err := nftMessageAttributes(message, unix.NFT_MSG_NEWSET)
	if err != nil {
		return nil, err
	}
	name, err := nftStringAttribute(attributes, unix.NFTA_SET_NAME)
	if err != nil {
		return nil, err
	}
	table, err := nftStringAttribute(attributes, unix.NFTA_SET_TABLE)
	if err != nil {
		return nil, err
	}
	set := &nftables.Set{Name: name, Table: &nftables.Table{Name: table, Family: nftables.TableFamilyINet}}
	flags, err := nftUint32Attribute(attributes, unix.NFTA_SET_FLAGS, true)
	if err != nil {
		return nil, err
	}
	set.Interval = flags&unix.NFT_SET_INTERVAL != 0
	kind, err := nftUint32Attribute(attributes, unix.NFTA_SET_KEY_TYPE, true)
	if err != nil {
		return nil, err
	}
	set.KeyType.SetNFTMagic(kind)
	set.KeyType.Bytes, err = nftUint32Attribute(attributes, unix.NFTA_SET_KEY_LEN, false)
	if err != nil {
		return nil, err
	}
	set.Comment, err = nftUserdataString(attributes[unix.NFTA_SET_USERDATA], byte(userdata.NFTNL_UDATA_SET_COMMENT))
	if err != nil {
		return nil, err
	}
	return &nftSetState{set: set, flags: flags}, nil
}

func decodeNFTElements(message wireMessage, name string) ([]nftables.SetElement, error) {
	attributes, err := nftMessageAttributes(message, unix.NFT_MSG_NEWSETELEM)
	if err != nil {
		return nil, err
	}
	table, err := nftStringAttribute(attributes, unix.NFTA_SET_ELEM_LIST_TABLE)
	if err != nil || table != nftTableName {
		return nil, errors.New("nftables returned elements from an unexpected table")
	}
	set, err := nftStringAttribute(attributes, unix.NFTA_SET_ELEM_LIST_SET)
	if err != nil || set != name {
		return nil, errors.New("nftables returned elements from an unexpected set")
	}
	items, err := parseWireAttributes(attributes[unix.NFTA_SET_ELEM_LIST_ELEMENTS])
	if err != nil {
		return nil, err
	}
	var elements []nftables.SetElement
	for _, item := range items {
		fields, err := nftAttributeMap(item.Data)
		if err != nil {
			return nil, err
		}
		key, err := nftAttributeMap(fields[unix.NFTA_SET_ELEM_KEY])
		if err != nil || len(key[unix.NFTA_DATA_VALUE]) == 0 {
			return nil, errors.New("invalid nftables set element key")
		}
		flags, err := nftUint32Attribute(fields, unix.NFTA_SET_ELEM_FLAGS, true)
		if err != nil {
			return nil, err
		}
		if flags & ^uint32(unix.NFT_SET_ELEM_INTERVAL_END) != 0 {
			return nil, errors.New("unsupported nftables managed set element flags")
		}
		comment, err := nftUserdataString(fields[unix.NFTA_SET_ELEM_USERDATA], byte(userdata.NFTNL_UDATA_SET_ELEM_COMMENT))
		if err != nil {
			return nil, err
		}
		elements = append(elements, nftables.SetElement{Key: append([]byte(nil), key[unix.NFTA_DATA_VALUE]...), IntervalEnd: flags&unix.NFT_SET_ELEM_INTERVAL_END != 0, Comment: comment})
	}
	return elements, nil
}

func decodeNFTRule(message wireMessage) (*nftRuleState, error) {
	attributes, err := nftMessageAttributes(message, unix.NFT_MSG_NEWRULE)
	if err != nil {
		return nil, err
	}
	table, err := nftStringAttribute(attributes, unix.NFTA_RULE_TABLE)
	if err != nil {
		return nil, err
	}
	chain, err := nftStringAttribute(attributes, unix.NFTA_RULE_CHAIN)
	if err != nil {
		return nil, err
	}
	handle := attributes[unix.NFTA_RULE_HANDLE]
	if len(handle) != 8 {
		return nil, errors.New("invalid nftables rule handle")
	}
	tableValue := &nftables.Table{Name: table, Family: nftables.TableFamilyINet}
	rule := &nftables.Rule{Table: tableValue, Chain: &nftables.Chain{Name: chain, Table: tableValue}, Handle: binary.BigEndian.Uint64(handle), UserData: attributes[unix.NFTA_RULE_USERDATA]}
	comment, err := nftUserdataString(rule.UserData, byte(userdata.TypeComment))
	if err != nil {
		return nil, err
	}
	items, err := parseWireAttributes(attributes[unix.NFTA_RULE_EXPRESSIONS])
	if err != nil {
		return nil, err
	}
	var fingerprint []byte
	for _, item := range items {
		value, err := canonicalNFTExpression(item.Data)
		if err != nil {
			return nil, err
		}
		fingerprint = appendWireAttribute(fingerprint, unix.NFTA_LIST_ELEM, value)
	}
	return &nftRuleState{rule: rule, comment: comment, expressions: fingerprint}, nil
}

func nftRuleFingerprint(rule *nftables.Rule) ([]byte, error) {
	var result []byte
	for _, expression := range rule.Exprs {
		data, err := expr.Marshal(byte(nftables.TableFamilyINet), expression)
		if err != nil {
			return nil, err
		}
		canonical, err := canonicalNFTExpression(data)
		if err != nil {
			return nil, err
		}
		result = appendWireAttribute(result, unix.NFTA_LIST_ELEM, canonical)
	}
	return result, nil
}

func canonicalNFTExpression(data []byte) ([]byte, error) {
	attributes, err := nftAttributeMap(data)
	if err != nil {
		return nil, err
	}
	name, err := nftStringAttribute(attributes, unix.NFTA_EXPR_NAME)
	if err != nil {
		return nil, err
	}
	fields, err := nftAttributeMap(attributes[unix.NFTA_EXPR_DATA])
	if err != nil {
		return nil, err
	}
	if name == "lookup" {
		// Set IDs are transaction-local references; the persistent name is
		// the identity returned by the kernel for an installed lookup.
		delete(fields, unix.NFTA_LOOKUP_SET_ID)
		// Linux nft_lookup_dump always emits flags, while the writer omits
		// the default zero value. Preserve inversion and unknown flag bits.
		flags, err := nftUint32Attribute(fields, unix.NFTA_LOOKUP_FLAGS, true)
		if err != nil {
			return nil, err
		}
		if flags == 0 {
			delete(fields, unix.NFTA_LOOKUP_FLAGS)
		}
	}
	if name == "socket" {
		key, err := nftUint32Attribute(fields, expr.NFTA_SOCKET_KEY, false)
		if err != nil {
			return nil, err
		}
		if key != expr.NFT_SOCKET_CGROUPV2 {
			delete(fields, expr.NFTA_SOCKET_LEVEL)
		}
	}
	if name == "counter" {
		delete(fields, unix.NFTA_COUNTER_BYTES)
		delete(fields, unix.NFTA_COUNTER_PACKETS)
	}
	if name == "immediate" || name == "cmp" {
		kind := uint16(unix.NFTA_IMMEDIATE_DATA)
		if name == "cmp" {
			kind = unix.NFTA_CMP_DATA
		}
		dataFields, err := nftAttributeMap(fields[kind])
		if err != nil {
			return nil, err
		}
		if verdict, present := dataFields[unix.NFTA_DATA_VERDICT]; present {
			verdictFields, err := nftAttributeMap(verdict)
			if err != nil {
				return nil, err
			}
			dataFields[unix.NFTA_DATA_VERDICT] = marshalNFTAttributes(verdictFields)
		}
		// Linux emits these nested containers without NLA_F_NESTED;
		// the Go library sets that hint. The attribute tree is identical.
		fields[kind] = marshalNFTAttributes(dataFields)
	}
	canonical := appendWireAttribute(nil, unix.NFTA_EXPR_NAME, []byte(name+"\x00"))
	return appendWireAttribute(canonical, unix.NFTA_EXPR_DATA, marshalNFTAttributes(fields)), nil
}

func marshalNFTAttributes(fields map[uint16][]byte) []byte {
	keys := make([]int, 0, len(fields))
	for kind := range fields {
		keys = append(keys, int(kind))
	}
	sort.Ints(keys)
	var values []byte
	for _, kind := range keys {
		values = appendWireAttribute(values, uint16(kind), fields[uint16(kind)])
	}
	return values
}

func nftUserdataString(data []byte, kind byte) (string, error) {
	var result string
	found := false
	for len(data) != 0 {
		if len(data) < 2 || int(data[1])+2 > len(data) {
			return "", fmt.Errorf("%w: invalid nftables userdata", errIncompleteNetlink)
		}
		length := int(data[1])
		if data[0] == kind {
			if found || length == 0 || data[length+1] != 0 {
				return "", fmt.Errorf("%w: invalid nftables comment", errIncompleteNetlink)
			}
			result, found = string(data[2:length+1]), true
		}
		data = data[length+2:]
	}
	return result, nil
}

//go:build linux

package transparentproxy

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/metacubex/nftables"
	"github.com/metacubex/nftables/binaryutil"
	"github.com/metacubex/nftables/expr"
	"github.com/metacubex/nftables/userdata"
	"golang.org/x/sys/unix"
)

// The pinned nftables.Conn cannot attach NFNL_BATCH_GENID. Its types and
// expression marshaler remain the rule representation; this narrowly scoped
// encoder adds the transaction precondition needed to protect ownership from
// an external table replacement between snapshot and commit.
func encodeNFTChanges(plan *nftChangePlan) ([]wireMessage, error) {
	var operations []wireMessage
	add := func(kind, flags uint16, attributes []byte) {
		operations = append(operations, wireMessage{
			Type:  uint16(unix.NFNL_SUBSYS_NFTABLES<<8) | kind,
			Flags: flags | unix.NLM_F_REQUEST | unix.NLM_F_ACK,
			Data:  append([]byte{unix.NFPROTO_INET, unix.NFNETLINK_V0, 0, 0}, attributes...),
		})
	}
	for _, rule := range plan.deleteRules {
		if rule.Handle == 0 {
			return nil, errors.New("refusing unscoped nftables rule deletion")
		}
		attributes := appendWireAttribute(nil, unix.NFTA_RULE_TABLE, []byte(nftTableName+"\x00"))
		attributes = appendWireAttribute(attributes, unix.NFTA_RULE_CHAIN, []byte(rule.Chain.Name+"\x00"))
		attributes = appendWireAttribute(attributes, unix.NFTA_RULE_HANDLE, binaryutil.BigEndian.PutUint64(rule.Handle))
		add(unix.NFT_MSG_DELRULE, 0, attributes)
	}
	for _, chain := range plan.deleteChains {
		attributes := appendWireAttribute(nil, unix.NFTA_CHAIN_TABLE, []byte(nftTableName+"\x00"))
		attributes = appendWireAttribute(attributes, unix.NFTA_CHAIN_NAME, []byte(chain.Name+"\x00"))
		// Never recursively remove rules injected into a managed chain.
		add(unix.NFT_MSG_DELCHAIN, unix.NLM_F_NONREC, attributes)
	}
	for _, set := range plan.deleteSets {
		attributes := appendWireAttribute(nil, unix.NFTA_SET_TABLE, []byte(nftTableName+"\x00"))
		attributes = appendWireAttribute(attributes, unix.NFTA_SET_NAME, []byte(set.Name+"\x00"))
		add(unix.NFT_MSG_DELSET, 0, attributes)
	}
	if plan.createTable != nil {
		attributes := appendWireAttribute(nil, unix.NFTA_TABLE_NAME, []byte(nftTableName+"\x00"))
		attributes = appendWireAttribute(attributes, unix.NFTA_TABLE_FLAGS, binaryutil.BigEndian.PutUint32(0))
		attributes = appendWireAttribute(attributes, nftTableUserdata, userdata.AppendString(nil, userdata.TypeComment, nftModule))
		add(unix.NFT_MSG_NEWTABLE, unix.NLM_F_CREATE|unix.NLM_F_EXCL, attributes)
	}
	for _, desired := range plan.createSets {
		set := desired.set
		attributes := appendWireAttribute(nil, unix.NFTA_SET_TABLE, []byte(nftTableName+"\x00"))
		attributes = appendWireAttribute(attributes, unix.NFTA_SET_NAME, []byte(set.Name+"\x00"))
		flags := uint32(0)
		if set.Interval {
			flags = unix.NFT_SET_INTERVAL
		}
		attributes = appendWireAttribute(attributes, unix.NFTA_SET_FLAGS, binaryutil.BigEndian.PutUint32(flags))
		attributes = appendWireAttribute(attributes, unix.NFTA_SET_KEY_TYPE, binaryutil.BigEndian.PutUint32(set.KeyType.GetNFTMagic()))
		attributes = appendWireAttribute(attributes, unix.NFTA_SET_KEY_LEN, binaryutil.BigEndian.PutUint32(set.KeyType.Bytes))
		attributes = appendWireAttribute(attributes, unix.NFTA_SET_ID, binaryutil.BigEndian.PutUint32(set.ID))
		var data []byte
		if set.Interval {
			data = userdata.AppendUint32(data, userdata.NFTNL_UDATA_SET_KEYBYTEORDER, 2)
		}
		data = userdata.AppendString(data, userdata.NFTNL_UDATA_SET_COMMENT, set.Comment)
		attributes = appendWireAttribute(attributes, unix.NFTA_SET_USERDATA, data)
		add(unix.NFT_MSG_NEWSET, unix.NLM_F_CREATE|unix.NLM_F_EXCL, attributes)
		elements, err := encodeNFTElements(set, desired.elements)
		if err != nil {
			return nil, err
		}
		for _, elementMessage := range elements {
			add(unix.NFT_MSG_NEWSETELEM, unix.NLM_F_CREATE, elementMessage)
		}
	}
	for _, chain := range plan.createChains {
		attributes := appendWireAttribute(nil, unix.NFTA_CHAIN_TABLE, []byte(nftTableName+"\x00"))
		attributes = appendWireAttribute(attributes, unix.NFTA_CHAIN_NAME, []byte(chain.Name+"\x00"))
		if chain.Hooknum != nil && chain.Priority != nil {
			hook := appendWireAttribute(nil, unix.NFTA_HOOK_HOOKNUM, binaryutil.BigEndian.PutUint32(uint32(*chain.Hooknum)))
			hook = appendWireAttribute(hook, unix.NFTA_HOOK_PRIORITY, binaryutil.BigEndian.PutUint32(uint32(*chain.Priority)))
			attributes = appendWireAttribute(attributes, unix.NFTA_CHAIN_HOOK|unix.NLA_F_NESTED, hook)
		}
		if chain.Type != "" {
			attributes = appendWireAttribute(attributes, unix.NFTA_CHAIN_TYPE, []byte(chain.Type+"\x00"))
		}
		if chain.Policy != nil {
			attributes = appendWireAttribute(attributes, unix.NFTA_CHAIN_POLICY, binaryutil.BigEndian.PutUint32(uint32(*chain.Policy)))
		}
		add(unix.NFT_MSG_NEWCHAIN, unix.NLM_F_CREATE|unix.NLM_F_EXCL, attributes)
	}
	for _, rule := range plan.createRules {
		attributes := appendWireAttribute(nil, unix.NFTA_RULE_TABLE, []byte(nftTableName+"\x00"))
		attributes = appendWireAttribute(attributes, unix.NFTA_RULE_CHAIN, []byte(rule.Chain.Name+"\x00"))
		var expressions []byte
		for _, expression := range rule.Exprs {
			data, err := expr.Marshal(byte(nftables.TableFamilyINet), expression)
			if err != nil {
				return nil, err
			}
			expressions = appendWireAttribute(expressions, unix.NFTA_LIST_ELEM|unix.NLA_F_NESTED, data)
		}
		if len(expressions) > 60<<10 {
			return nil, errors.New("transparent-proxy rule exceeds nftables attribute limit")
		}
		attributes = appendWireAttribute(attributes, unix.NFTA_RULE_EXPRESSIONS|unix.NLA_F_NESTED, expressions)
		attributes = appendWireAttribute(attributes, unix.NFTA_RULE_USERDATA, rule.UserData)
		add(unix.NFT_MSG_NEWRULE, unix.NLM_F_CREATE|unix.NLM_F_APPEND, attributes)
	}
	for _, desired := range plan.updateSets {
		attributes := appendWireAttribute(nil, unix.NFTA_SET_ELEM_LIST_TABLE, []byte(nftTableName+"\x00"))
		attributes = appendWireAttribute(attributes, unix.NFTA_SET_ELEM_LIST_SET, []byte(desired.set.Name+"\x00"))
		add(unix.NFT_MSG_DELSETELEM, 0, attributes)
		elements, err := encodeNFTElements(desired.set, desired.elements)
		if err != nil {
			return nil, err
		}
		for _, elementMessage := range elements {
			add(unix.NFT_MSG_NEWSETELEM, unix.NLM_F_CREATE, elementMessage)
		}
	}
	if plan.deleteTable != nil {
		// A foreign chain, set, object or flowtable must keep its container.
		attributes := appendWireAttribute(nil, unix.NFTA_TABLE_NAME, []byte(nftTableName+"\x00"))
		add(unix.NFT_MSG_DELTABLE, unix.NLM_F_NONREC, attributes)
	}
	return operations, nil
}

func encodeNFTElements(set *nftables.Set, elements []nftables.SetElement) ([][]byte, error) {
	var messages [][]byte
	var list []byte
	flush := func() {
		if len(list) == 0 {
			return
		}
		attributes := appendWireAttribute(nil, unix.NFTA_SET_ELEM_LIST_TABLE, []byte(nftTableName+"\x00"))
		attributes = appendWireAttribute(attributes, unix.NFTA_SET_ELEM_LIST_SET, []byte(set.Name+"\x00"))
		attributes = appendWireAttribute(attributes, unix.NFTA_SET_ELEM_LIST_ELEMENTS|unix.NLA_F_NESTED, list)
		messages = append(messages, attributes)
		list = nil
	}
	for _, element := range elements {
		if len(element.Key) != int(set.KeyType.Bytes) {
			return nil, fmt.Errorf("invalid key length in nftables set %s", set.Name)
		}
		key := appendWireAttribute(nil, unix.NFTA_DATA_VALUE, element.Key)
		item := appendWireAttribute(nil, unix.NFTA_SET_ELEM_KEY|unix.NLA_F_NESTED, key)
		if element.IntervalEnd {
			item = appendWireAttribute(item, unix.NFTA_SET_ELEM_FLAGS, binaryutil.BigEndian.PutUint32(unix.NFT_SET_ELEM_INTERVAL_END))
		}
		if element.Comment != "" {
			comment := userdata.AppendString(nil, userdata.NFTNL_UDATA_SET_ELEM_COMMENT, element.Comment)
			item = appendWireAttribute(item, unix.NFTA_SET_ELEM_USERDATA, comment)
		}
		if len(list)+len(item)+4 > 48<<10 {
			flush()
		}
		list = appendWireAttribute(list, unix.NFTA_LIST_ELEM|unix.NLA_F_NESTED, item)
	}
	flush()
	return messages, nil
}

func writeNFTBatch(ctx context.Context, generation uint32, operations []wireMessage) error {
	if len(operations) == 0 {
		return nil
	}
	connection, err := newWireConn(unix.NETLINK_NETFILTER, 0)
	if err != nil {
		return err
	}
	defer connection.Close()
	return executeNFTBatch(ctx, connection, generation, operations)
}

// All operations travel in one nfnetlink transaction. The kernel validates
// GENID under its commit mutex before applying any operation; stale snapshots
// fail with ERESTART without deleting or replacing newly observed objects.
// Expose that as EAGAIN to the module's bounded reread path.
func executeNFTBatch(ctx context.Context, connection *wireConn, generation uint32, operations []wireMessage) error {
	if len(operations) == 0 {
		return nil
	}
	if generation == 0 {
		return errors.New("refusing an nftables transaction without a generation guard")
	}
	connection.mu.Lock()
	defer connection.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, wireRequestTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	nextSequence := func() uint32 {
		connection.sequence++
		if connection.sequence == 0 {
			connection.sequence++
		}
		return connection.sequence
	}
	begin := appendWireAttribute([]byte{unix.NFPROTO_UNSPEC, unix.NFNETLINK_V0, 0, unix.NFNL_SUBSYS_NFTABLES}, unix.NFNL_BATCH_GENID, binaryutil.BigEndian.PutUint32(generation))
	data := marshalWirePacket(unix.NFNL_MSG_BATCH_BEGIN, unix.NLM_F_REQUEST, nextSequence(), connection.pid, begin)
	pending := make(map[uint32]bool, len(operations))
	for _, operation := range operations {
		if operation.Type>>8 != unix.NFNL_SUBSYS_NFTABLES {
			return errors.New("invalid subsystem in nftables transaction")
		}
		sequence := nextSequence()
		pending[sequence] = true
		data = append(data, marshalWirePacket(operation.Type, operation.Flags|unix.NLM_F_REQUEST|unix.NLM_F_ACK, sequence, connection.pid, operation.Data)...)
	}
	end := []byte{unix.NFPROTO_UNSPEC, unix.NFNETLINK_V0, 0, unix.NFNL_SUBSYS_NFTABLES}
	data = append(data, marshalWirePacket(unix.NFNL_MSG_BATCH_END, unix.NLM_F_REQUEST, nextSequence(), connection.pid, end)...)
	if len(data) > wireMaxDatagram {
		return errors.New("transparent-proxy nftables transaction exceeds datagram limit")
	}
	n, err := connection.socket.Sendmsg(ctx, data, nil, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}, 0)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	for len(pending) != 0 {
		packets, err := connection.receive(ctx)
		if err != nil {
			if errors.Is(err, unix.ERESTART) {
				return fmt.Errorf("nftables generation changed: %w", errors.Join(unix.EAGAIN, err))
			}
			return err
		}
		for _, packet := range packets {
			if packet.Type != unix.NLMSG_ERROR || !pending[packet.sequence] || packet.pid != 0 && packet.pid != connection.pid {
				return fmt.Errorf("%w: unexpected nftables batch acknowledgement", errIncompleteNetlink)
			}
			delete(pending, packet.sequence)
		}
	}
	return nil
}

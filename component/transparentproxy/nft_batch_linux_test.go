//go:build linux

package transparentproxy

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/metacubex/nftables"
	"github.com/metacubex/nftables/binaryutil"
	"golang.org/x/sys/unix"
)

type nftBatchTestSocket struct {
	sent      [][]byte
	replies   [][]byte
	onSend    func([]wirePacket) [][]byte
	truncated bool
}

func (s *nftBatchTestSocket) Sendmsg(_ context.Context, data, _ []byte, _ unix.Sockaddr, _ int) (int, error) {
	s.sent = append(s.sent, append([]byte(nil), data...))
	packets, err := parseWirePackets(data)
	if err != nil {
		return 0, err
	}
	s.replies = append(s.replies, s.onSend(packets)...)
	return len(data), nil
}

func (s *nftBatchTestSocket) Recvmsg(_ context.Context, data, _ []byte, flags int) (int, int, int, unix.Sockaddr, error) {
	if len(s.replies) == 0 {
		return 0, 0, 0, nil, io.EOF
	}
	reply := s.replies[0]
	copy(data, reply)
	if flags&unix.MSG_PEEK != 0 {
		return len(reply), 0, unix.MSG_TRUNC, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}, nil
	}
	s.replies = s.replies[1:]
	receivedFlags := 0
	if s.truncated {
		receivedFlags = unix.MSG_TRUNC
	}
	return len(reply), 0, receivedFlags, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}, nil
}

func (s *nftBatchTestSocket) Close() error { return nil }

func nftBatchAck(sequence uint32, status int32) []byte {
	return marshalWirePacket(unix.NLMSG_ERROR, 0, sequence, 0, binaryutil.NativeEndian.PutUint32(uint32(status)))
}

func TestNFTBatchGenerationAndCompleteAcknowledgements(t *testing.T) {
	program, err := buildNFTRuleset("batch-owner", nftTestState())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planNFTChanges(&nftSnapshot{generation: 42}, program)
	if err != nil {
		t.Fatal(err)
	}
	operations, err := encodeNFTChanges(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) < 20 {
		t.Fatal("test needs a multi-operation transaction")
	}
	socket := &nftBatchTestSocket{onSend: func(packets []wirePacket) [][]byte {
		if len(packets) != len(operations)+2 || packets[0].Type != unix.NFNL_MSG_BATCH_BEGIN || packets[len(packets)-1].Type != unix.NFNL_MSG_BATCH_END {
			t.Fatal("operations were not enclosed in one batch")
		}
		attributes, err := nftAttributeMap(packets[0].Data[4:])
		if err != nil {
			t.Fatal(err)
		}
		generation, err := nftUint32Attribute(attributes, unix.NFNL_BATCH_GENID, false)
		if err != nil || generation != 42 {
			t.Fatalf("missing generation precondition: %d %v", generation, err)
		}
		var replies [][]byte
		for _, packet := range packets[1 : len(packets)-1] {
			if packet.Flags&unix.NLM_F_ACK == 0 {
				t.Fatal("operation was not acknowledged")
			}
			replies = append(replies, nftBatchAck(packet.sequence, 0))
		}
		return replies
	}}
	if err := executeNFTBatch(context.Background(), &wireConn{socket: socket, pid: 50}, 42, operations); err != nil {
		t.Fatal(err)
	}
	if len(socket.sent) != 1 || len(socket.replies) != 0 {
		t.Fatal("transaction split or acknowledged only partially")
	}

	for _, test := range []struct {
		name      string
		respond   func([]wirePacket) [][]byte
		truncated bool
		want      error
	}{
		{name: "generation-changed", want: unix.EAGAIN, respond: func(p []wirePacket) [][]byte { return [][]byte{nftBatchAck(p[0].sequence, -int32(unix.ERESTART))} }},
		{name: "operation-denied", want: unix.EPERM, respond: func(p []wirePacket) [][]byte { return [][]byte{nftBatchAck(p[1].sequence, -int32(unix.EPERM))} }},
		{name: "missing-ack", want: io.EOF, respond: func(p []wirePacket) [][]byte { return [][]byte{nftBatchAck(p[1].sequence, 0)} }},
		{name: "duplicate-ack", want: errIncompleteNetlink, respond: func(p []wirePacket) [][]byte {
			return [][]byte{append(nftBatchAck(p[1].sequence, 0), nftBatchAck(p[1].sequence, 0)...)}
		}},
		{name: "unrelated-sequence", want: errIncompleteNetlink, respond: func(p []wirePacket) [][]byte { return [][]byte{nftBatchAck(p[len(p)-1].sequence+1, 0)} }},
		{name: "truncated-ack", want: errIncompleteNetlink, truncated: true, respond: func(p []wirePacket) [][]byte { return [][]byte{nftBatchAck(p[1].sequence, 0)} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			socket := &nftBatchTestSocket{onSend: test.respond, truncated: test.truncated}
			err := executeNFTBatch(context.Background(), &wireConn{socket: socket, pid: 50}, 42, operations)
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v want %v", err, test.want)
			}
		})
	}
	if err := executeNFTBatch(context.Background(), &wireConn{}, 0, operations); err == nil {
		t.Fatal("sent an unguarded generation-zero batch")
	}
}

func TestNFTGenerationRetryRereadsOwnership(t *testing.T) {
	state := nftTestState()
	program, err := buildNFTRuleset("owner", state)
	if err != nil {
		t.Fatal(err)
	}
	actual := nftSnapshotFromProgram(t, program)
	state.Tun4 = false
	reads, writes := 0, 0
	f := &nftFirewall{owner: "owner"}
	f.read = func(context.Context, string) (*nftSnapshot, error) { reads++; return actual, nil }
	f.write = func(_ context.Context, generation uint32, _ []wireMessage) error {
		writes++
		if generation != actual.generation {
			t.Fatal("reused stale generation after reread")
		}
		actual = &nftSnapshot{generation: generation + 1, table: program.table, chains: map[string]*nftChainState{}, sets: map[string]*nftSetState{}}
		return unix.EAGAIN
	}
	if err := f.Apply(context.Background(), state); err == nil {
		t.Fatal("applied stale plan to externally replaced table")
	}
	if reads != 2 || writes != 1 {
		t.Fatalf("did not revalidate owner after generation failure: reads=%d writes=%d", reads, writes)
	}

	actual = nftSnapshotFromProgram(t, program)
	reads, writes = 0, 0
	f.write = func(context.Context, uint32, []wireMessage) error { writes++; return unix.EAGAIN }
	if err := f.Apply(context.Background(), state); !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("busy ruleset: %v", err)
	}
	if reads != 2 || writes != 2 {
		t.Fatalf("unbounded generation retry: reads=%d writes=%d", reads, writes)
	}
}

func TestNFTBatchEncodingMatchesSnapshot(t *testing.T) {
	program, err := buildNFTRuleset("encode-owner", nftTestState())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planNFTChanges(&nftSnapshot{generation: 1}, program)
	if err != nil {
		t.Fatal(err)
	}
	operations, err := encodeNFTChanges(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range operations {
		switch operation.Type & 0xff {
		case unix.NFT_MSG_NEWCHAIN:
			chain, err := decodeNFTChain(operation)
			if err != nil {
				t.Fatal(err)
			}
			if chain.chain.Hooknum != nil && *chain.chain.Priority != -150 {
				t.Fatal("capture does not run at mangle priority")
			}
		case unix.NFT_MSG_NEWSET:
			set, err := decodeNFTSet(operation)
			if err != nil {
				t.Fatal(err)
			}
			if set.set.Comment != program.owner+"/set/"+set.set.Name {
				t.Fatal("lost set owner")
			}
		case unix.NFT_MSG_NEWSETELEM:
			attributes, err := nftMessageAttributes(operation, unix.NFT_MSG_NEWSETELEM)
			if err != nil {
				t.Fatal(err)
			}
			name, err := nftStringAttribute(attributes, unix.NFTA_SET_ELEM_LIST_SET)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeNFTElements(operation, name); err != nil {
				t.Fatal(err)
			}
		case unix.NFT_MSG_NEWRULE:
			operation.Data = appendWireAttribute(operation.Data, unix.NFTA_RULE_HANDLE, binaryutil.BigEndian.PutUint64(1))
			if _, err := decodeNFTRule(operation); err != nil {
				t.Fatal(err)
			}
		}
	}
	set := &nftables.Set{Name: "many", KeyType: nftables.TypeInteger}
	var elements []nftables.SetElement
	for i := uint32(0); i < 10000; i++ {
		elements = append(elements, nftables.SetElement{Key: binaryutil.NativeEndian.PutUint32(i)})
	}
	messages, err := encodeNFTElements(set, elements)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) < 2 {
		t.Fatal("large element list was not split before the netlink attribute limit")
	}
	count := 0
	for _, data := range messages {
		message := wireMessage{Type: uint16(unix.NFNL_SUBSYS_NFTABLES<<8) | unix.NFT_MSG_NEWSETELEM, Data: append([]byte{unix.NFPROTO_INET, 0, 0, 0}, data...)}
		decoded, err := decodeNFTElements(message, "many")
		if err != nil {
			t.Fatal(err)
		}
		count += len(decoded)
	}
	if count != len(elements) {
		t.Fatal("element chunking lost data")
	}
}

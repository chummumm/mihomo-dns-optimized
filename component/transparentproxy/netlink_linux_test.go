//go:build linux

package transparentproxy

import (
	"context"
	"errors"
	"io"
	"testing"

	"golang.org/x/sys/unix"
)

type wireTestDatagram struct {
	data  []byte
	flags int
	pid   uint32
	err   error
}

type wireTestSocket struct {
	queue  []wireTestDatagram
	onSend func(wirePacket) []wirePacket
	closed bool
}

func (s *wireTestSocket) Sendmsg(ctx context.Context, data, _ []byte, _ unix.Sockaddr, _ int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	packets, err := parseWirePackets(data)
	if err != nil {
		return 0, err
	}
	for _, request := range packets {
		if s.onSend == nil {
			continue
		}
		var reply []byte
		for _, response := range s.onSend(request) {
			reply = append(reply, marshalWirePacket(response.Type, response.Flags, request.sequence, request.pid, response.Data)...)
		}
		if len(reply) != 0 {
			s.queue = append(s.queue, wireTestDatagram{data: reply})
		}
	}
	return len(data), nil
}

func (s *wireTestSocket) Recvmsg(ctx context.Context, data, _ []byte, flags int) (int, int, int, unix.Sockaddr, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, 0, nil, err
	}
	if len(s.queue) == 0 {
		return 0, 0, 0, nil, io.EOF
	}
	packet := s.queue[0]
	if packet.err != nil {
		s.queue = s.queue[1:]
		return 0, 0, 0, nil, packet.err
	}
	copy(data, packet.data)
	if flags&unix.MSG_PEEK != 0 {
		return len(packet.data), 0, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Pid: packet.pid}, nil
	}
	s.queue = s.queue[1:]
	return len(packet.data), 0, packet.flags, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Pid: packet.pid}, nil
}

func (s *wireTestSocket) Close() error { s.closed = true; return nil }

func testWireConnection(handler func(wirePacket) []wirePacket) *wireConn {
	open := func() (wireSocket, uint32, error) { return &wireTestSocket{onSend: handler}, 42, nil }
	s, pid, _ := open()
	return &wireConn{socket: s, pid: pid, open: open}
}

func testWireAck(err error) wirePacket {
	status := uint32(0)
	if err != nil {
		var errno unix.Errno
		if !errors.As(err, &errno) {
			panic("test ack requires errno")
		}
		status = uint32(-int32(errno))
	}
	return wirePacket{wireMessage: wireMessage{Type: unix.NLMSG_ERROR, Data: wireUint32(status)}}
}

func testWireDump(messages ...wireMessage) []wirePacket {
	packets := make([]wirePacket, 0, len(messages)+1)
	for _, message := range messages {
		message.Flags |= unix.NLM_F_MULTI
		packets = append(packets, wirePacket{wireMessage: message})
	}
	return append(packets, wirePacket{wireMessage: wireMessage{Type: unix.NLMSG_DONE, Data: wireUint32(0)}})
}

func TestWireDumpRequiresCompleteUninterruptedReply(t *testing.T) {
	for _, test := range []struct {
		name    string
		replies []wirePacket
		want    error
	}{
		{"complete", testWireDump(wireMessage{Type: unix.RTM_NEWLINK, Data: []byte("value")}), nil},
		{"interrupted-done", []wirePacket{{wireMessage: wireMessage{Type: unix.NLMSG_DONE, Flags: unix.NLM_F_DUMP_INTR, Data: wireUint32(0)}}}, errIncompleteNetlink},
		{"negative-done", []wirePacket{{wireMessage: wireMessage{Type: unix.NLMSG_DONE, Data: wireUint32(uint32(0xfffffff4))}}}, unix.ENOMEM},
		{"overrun", []wirePacket{{wireMessage: wireMessage{Type: unix.NLMSG_OVERRUN}}}, errIncompleteNetlink},
		{"partial-no-done", []wirePacket{{wireMessage: wireMessage{Type: unix.RTM_NEWLINK, Flags: unix.NLM_F_MULTI, Data: []byte("value")}}}, io.EOF},
		{"after-done", []wirePacket{{wireMessage: wireMessage{Type: unix.NLMSG_DONE, Data: wireUint32(0)}}, {wireMessage: wireMessage{Type: unix.RTM_NEWLINK, Data: []byte("value")}}}, errIncompleteNetlink},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := testWireConnection(func(wirePacket) []wirePacket { return test.replies })
			defer conn.Close()
			result, err := conn.Execute(context.Background(), unix.RTM_GETLINK, unix.NLM_F_DUMP, nil)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if test.want != nil && result != nil {
				t.Fatal("exposed partial snapshot after failure")
			}
			if test.want == nil && (len(result) != 1 || string(result[0].Data) != "value") {
				t.Fatalf("bad complete result: %+v", result)
			}
		})
	}
}

func TestWireReceiveRejectsLossAndWrongPeer(t *testing.T) {
	valid := marshalWirePacket(unix.RTM_NEWLINK, 0, 0, 0, []byte("value"))
	for _, packet := range []wireTestDatagram{
		{data: valid, flags: unix.MSG_TRUNC},
		{data: valid, flags: unix.MSG_CTRUNC},
		{data: valid, pid: 999},
		{err: unix.ENOBUFS},
		{data: []byte{1, 2, 3}},
	} {
		if result, err := receiveWirePackets(context.Background(), &wireTestSocket{queue: []wireTestDatagram{packet}}); err == nil || result != nil {
			t.Fatalf("accepted incomplete notification: %+v, %v", result, err)
		}
	}
}

func TestWireFailureReopensBeforeNextRequest(t *testing.T) {
	old := &wireTestSocket{queue: []wireTestDatagram{{err: unix.ENOBUFS}}}
	reopened := 0
	conn := &wireConn{socket: old, pid: 12, open: func() (wireSocket, uint32, error) {
		reopened++
		return &wireTestSocket{onSend: func(wirePacket) []wirePacket {
			return testWireDump(wireMessage{Type: unix.RTM_NEWLINK, Data: []byte("fresh")})
		}}, 99, nil
	}}
	defer conn.Close()
	if _, err := conn.Execute(context.Background(), unix.RTM_GETLINK, unix.NLM_F_DUMP, nil); !errors.Is(err, unix.ENOBUFS) {
		t.Fatal(err)
	}
	if !old.closed {
		t.Fatal("failed request socket remained open")
	}
	result, err := conn.Execute(context.Background(), unix.RTM_GETLINK, unix.NLM_F_DUMP, nil)
	if err != nil || reopened != 1 || len(result) != 1 || string(result[0].Data) != "fresh" {
		t.Fatalf("fresh request: %+v, %v, opens=%d", result, err, reopened)
	}
}

func TestWireRejectsMalformedAttributesAndWrongSequence(t *testing.T) {
	for _, data := range [][]byte{{1}, {3, 0, 1, 0}, {8, 0, 1, 0, 0}} {
		if _, err := parseWireAttributes(data); err == nil {
			t.Fatalf("accepted malformed attribute: %x", data)
		}
	}
	conn := &wireConn{socket: &wireTestSocket{queue: []wireTestDatagram{{data: marshalWirePacket(unix.NLMSG_DONE, 0, 99, 42, wireUint32(0))}}}, pid: 42}
	defer conn.Close()
	if _, err := conn.Execute(context.Background(), unix.RTM_GETLINK, unix.NLM_F_DUMP, nil); !errors.Is(err, errIncompleteNetlink) {
		t.Fatalf("wrong sequence: %v", err)
	}
}

func TestSystemNotificationWaitsForNFTTransactionAndAcceptsNetconf(t *testing.T) {
	data := appendWireAttribute(make([]byte, 4), unix.NFTA_GEN_ID, []byte{0, 0, 0, 7})
	changed, err := validateSystemNotification(wirePacket{wireMessage: wireMessage{Type: unix.NFNL_SUBSYS_NFTABLES<<8 | unix.NFT_MSG_NEWGEN, Data: data}}, true)
	if err != nil || !changed {
		t.Fatalf("transaction completion: changed=%v err=%v", changed, err)
	}
	changed, err = validateSystemNotification(wirePacket{wireMessage: wireMessage{Type: unix.NFNL_SUBSYS_NFTABLES<<8 | unix.NFT_MSG_NEWRULE, Data: data}}, true)
	if err != nil || changed {
		t.Fatalf("partial transaction must not be applied: %v %v", changed, err)
	}
	if _, err := validateSystemNotification(wirePacket{wireMessage: wireMessage{Type: unix.NFNL_SUBSYS_NFTABLES<<8 | unix.NFT_MSG_NEWGEN, Data: make([]byte, 4)}}, true); err == nil {
		t.Fatal("accepted generation without ID")
	}
	changed, err = validateSystemNotification(wirePacket{wireMessage: wireMessage{Type: unix.RTM_NEWNETCONF, Data: appendWireAttribute(make([]byte, 4), 1, wireUint32(2))}}, false)
	if err != nil || !changed {
		t.Fatalf("netconf event: %v %v", changed, err)
	}
}

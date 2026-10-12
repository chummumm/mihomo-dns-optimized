//go:build linux

package transparentproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/mdlayher/socket"
	"github.com/sagernet/netlink/nl"
	"golang.org/x/sys/unix"
)

var wireOrder = nl.NativeEndian()

var errIncompleteNetlink = errors.New("incomplete netlink observation")

const (
	wireAttributeTypeMask uint16 = 0x3fff
	wireHeaderSize               = 16
	wireMaxDatagram              = 16 << 20
	wireMaxDump                  = 128 << 20
	wireRequestTimeout           = 10 * time.Second
)

// wireMessage deliberately retains DONE until Execute has checked its flags
// and status. Some higher-level netlink readers discard an interrupted DONE.
type wireMessage struct {
	Type  uint16
	Flags uint16
	Data  []byte
}

type wirePacket struct {
	wireMessage
	sequence uint32
	pid      uint32
}

type wireSocket interface {
	Recvmsg(context.Context, []byte, []byte, int) (int, int, int, unix.Sockaddr, error)
	Sendmsg(context.Context, []byte, []byte, unix.Sockaddr, int) (int, error)
	Close() error
}

// Each connection is either a request connection or a notification connection.
// Execute serializes request/reply exchanges. Notification readers use receive
// on their own sockets, so unsolicited messages cannot corrupt a dump.
type wireConn struct {
	mu       sync.Mutex
	socketMu sync.Mutex
	socket   wireSocket
	pid      uint32
	sequence uint32
	open     func() (wireSocket, uint32, error)
	reset    bool
	closed   bool
}

func newWireConn(protocol int, groups uint32) (*wireConn, error) {
	open := func() (wireSocket, uint32, error) { return openWireSocket(protocol, groups) }
	s, pid, err := open()
	if err != nil {
		return nil, err
	}
	return &wireConn{socket: s, pid: pid, open: open}, nil
}

func openWireSocket(protocol int, groups uint32) (wireSocket, uint32, error) {
	s, err := socket.Socket(unix.AF_NETLINK, unix.SOCK_RAW, protocol, "transparent-proxy-netlink", nil)
	if err != nil {
		return nil, 0, err
	}
	fail := func(err error) (wireSocket, uint32, error) { _ = s.Close(); return nil, 0, err }
	if err := s.SetsockoptInt(unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<20); err != nil {
		return fail(err)
	}
	if err := s.Bind(&unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: groups}); err != nil {
		return fail(err)
	}
	address, err := s.Getsockname()
	if err != nil {
		return fail(err)
	}
	nlAddress, ok := address.(*unix.SockaddrNetlink)
	if !ok {
		return fail(errors.New("unexpected netlink socket address"))
	}
	return s, nlAddress.Pid, nil
}

func (c *wireConn) Close() error {
	c.socketMu.Lock()
	defer c.socketMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.socket.Close()
}

func (c *wireConn) requestSocket() (wireSocket, uint32, error) {
	c.socketMu.Lock()
	defer c.socketMu.Unlock()
	if c.closed {
		return nil, 0, net.ErrClosed
	}
	if c.reset {
		if c.open == nil {
			return nil, 0, net.ErrClosed
		}
		s, pid, err := c.open()
		if err != nil {
			return nil, 0, err
		}
		c.socket, c.pid, c.reset = s, pid, false
	}
	return c.socket, c.pid, nil
}

func (c *wireConn) Execute(ctx context.Context, messageType uint16, flags uint16, payload []byte) ([]wireMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, wireRequestTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, pid, err := c.requestSocket()
	if err != nil {
		return nil, err
	}
	c.sequence++
	if c.sequence == 0 {
		c.sequence++
	}
	result, err := c.exchange(ctx, s, pid, messageType, flags, payload)
	if err != nil {
		// A failed exchange may leave replies (including DONE) queued. A new
		// socket gives the next snapshot a fresh request boundary, without
		// guessing which stale messages would be safe to discard.
		c.socketMu.Lock()
		if !c.closed {
			c.reset = true
			_ = s.Close()
		}
		c.socketMu.Unlock()
	}
	return result, err
}

func (c *wireConn) exchange(ctx context.Context, s wireSocket, pid uint32, messageType uint16, flags uint16, payload []byte) ([]wireMessage, error) {
	request := marshalWirePacket(messageType, flags|unix.NLM_F_REQUEST, c.sequence, pid, payload)
	n, err := s.Sendmsg(ctx, request, nil, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}, 0)
	if err != nil {
		return nil, err
	}
	if n != len(request) {
		return nil, io.ErrShortWrite
	}
	var result []wireMessage
	total := 0
	dump := flags&unix.NLM_F_DUMP == unix.NLM_F_DUMP
	ack := flags&unix.NLM_F_ACK != 0
	for {
		packets, err := receiveWirePackets(ctx, s)
		if err != nil {
			return nil, err
		}
		complete := false
		for _, packet := range packets {
			if packet.sequence != c.sequence || packet.pid != 0 && packet.pid != pid {
				return nil, fmt.Errorf("%w: unexpected request sequence or recipient", errIncompleteNetlink)
			}
			if err := checkWirePacket(packet); err != nil {
				return nil, err
			}
			switch packet.Type {
			case unix.NLMSG_NOOP:
				continue
			case unix.NLMSG_ERROR:
				if !dump {
					complete = true
				}
			case unix.NLMSG_DONE:
				complete = true
			default:
				if complete {
					return nil, fmt.Errorf("%w: data after completion", errIncompleteNetlink)
				}
				total += len(packet.Data)
				if total > wireMaxDump {
					return nil, fmt.Errorf("%w: dump exceeds limit", errIncompleteNetlink)
				}
				result = append(result, packet.wireMessage)
				if !dump && !ack && packet.Flags&unix.NLM_F_MULTI == 0 {
					complete = true
				}
			}
		}
		if complete {
			return result, nil
		}
	}
}

func (c *wireConn) receive(ctx context.Context) ([]wirePacket, error) {
	c.socketMu.Lock()
	s := c.socket
	closed := c.closed
	c.socketMu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	return receiveWirePackets(ctx, s)
}

func receiveWirePackets(ctx context.Context, s wireSocket) ([]wirePacket, error) {
	// MSG_PEEK|MSG_TRUNC reports the complete datagram length without consuming
	// it. Only this reader accesses the socket; the second read must fit exactly.
	peek := make([]byte, 1)
	n, _, peekFlags, _, err := s.Recvmsg(ctx, peek, nil, unix.MSG_PEEK|unix.MSG_TRUNC)
	if err != nil {
		return nil, err
	}
	if n <= 0 || n > wireMaxDatagram || peekFlags&unix.MSG_CTRUNC != 0 {
		return nil, fmt.Errorf("%w: invalid datagram length or ancillary truncation", errIncompleteNetlink)
	}
	buffer := make([]byte, n)
	n, _, flags, from, err := s.Recvmsg(ctx, buffer, nil, 0)
	if err != nil {
		return nil, err
	}
	if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || n <= 0 || n > len(buffer) {
		return nil, fmt.Errorf("%w: truncated datagram", errIncompleteNetlink)
	}
	address, ok := from.(*unix.SockaddrNetlink)
	if !ok || address.Pid != 0 {
		return nil, fmt.Errorf("%w: notification is not from the kernel", errIncompleteNetlink)
	}
	return parseWirePackets(buffer[:n])
}

func parseWirePackets(data []byte) ([]wirePacket, error) {
	var packets []wirePacket
	for len(data) != 0 {
		if len(data) < wireHeaderSize {
			return nil, fmt.Errorf("%w: short header", errIncompleteNetlink)
		}
		length := int(wireOrder.Uint32(data[:4]))
		if length < wireHeaderSize || length > len(data) {
			return nil, fmt.Errorf("%w: invalid header length", errIncompleteNetlink)
		}
		packet := wirePacket{
			wireMessage: wireMessage{Type: wireOrder.Uint16(data[4:6]), Flags: wireOrder.Uint16(data[6:8]), Data: data[wireHeaderSize:length]},
			sequence:    wireOrder.Uint32(data[8:12]), pid: wireOrder.Uint32(data[12:16]),
		}
		if err := checkWirePacket(packet); err != nil {
			return nil, err
		}
		packets = append(packets, packet)
		aligned := wireAlign(length)
		if aligned > len(data) {
			if length == len(data) {
				break
			}
			return nil, fmt.Errorf("%w: missing message alignment", errIncompleteNetlink)
		}
		data = data[aligned:]
	}
	if len(packets) == 0 {
		return nil, fmt.Errorf("%w: empty datagram", errIncompleteNetlink)
	}
	return packets, nil
}

func checkWirePacket(packet wirePacket) error {
	if packet.Flags&unix.NLM_F_DUMP_INTR != 0 || packet.Type == unix.NLMSG_OVERRUN {
		return fmt.Errorf("%w: interrupted dump or overrun", errIncompleteNetlink)
	}
	if packet.Type != unix.NLMSG_ERROR && packet.Type != unix.NLMSG_DONE {
		return nil
	}
	if packet.Type == unix.NLMSG_DONE && len(packet.Data) == 0 {
		return nil
	}
	if len(packet.Data) < 4 {
		return fmt.Errorf("%w: short completion status", errIncompleteNetlink)
	}
	status := int32(wireOrder.Uint32(packet.Data[:4]))
	if status < 0 {
		return unix.Errno(-int64(status))
	}
	if status != 0 {
		return fmt.Errorf("%w: invalid positive completion status", errIncompleteNetlink)
	}
	return nil
}

func marshalWirePacket(messageType, flags uint16, sequence, pid uint32, payload []byte) []byte {
	data := make([]byte, wireAlign(wireHeaderSize+len(payload)))
	wireOrder.PutUint32(data[:4], uint32(wireHeaderSize+len(payload)))
	wireOrder.PutUint16(data[4:6], messageType)
	wireOrder.PutUint16(data[6:8], flags)
	wireOrder.PutUint32(data[8:12], sequence)
	wireOrder.PutUint32(data[12:16], pid)
	copy(data[wireHeaderSize:], payload)
	return data
}

func wireAlign(n int) int { return (n + 3) &^ 3 }

type wireAttribute struct {
	Type uint16
	Data []byte
}

func parseWireAttributes(data []byte) ([]wireAttribute, error) {
	var attributes []wireAttribute
	for len(data) != 0 {
		if len(data) < 4 {
			return nil, fmt.Errorf("%w: short attribute", errIncompleteNetlink)
		}
		length := int(wireOrder.Uint16(data[:2]))
		if length < 4 || length > len(data) {
			return nil, fmt.Errorf("%w: invalid attribute length", errIncompleteNetlink)
		}
		attributes = append(attributes, wireAttribute{Type: wireOrder.Uint16(data[2:4]), Data: data[4:length]})
		aligned := wireAlign(length)
		if aligned > len(data) {
			if length == len(data) {
				break
			}
			return nil, fmt.Errorf("%w: missing attribute alignment", errIncompleteNetlink)
		}
		data = data[aligned:]
	}
	return attributes, nil
}

func appendWireAttribute(data []byte, kind uint16, value []byte) []byte {
	start := len(data)
	data = append(data, make([]byte, wireAlign(4+len(value)))...)
	wireOrder.PutUint16(data[start:start+2], uint16(4+len(value)))
	wireOrder.PutUint16(data[start+2:start+4], kind)
	copy(data[start+4:], value)
	return data
}

func wireUint32(value uint32) []byte {
	data := make([]byte, 4)
	wireOrder.PutUint32(data, value)
	return data
}

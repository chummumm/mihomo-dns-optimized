package dns

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/component/dnsmessage"
	D "github.com/miekg/dns"
)

// An invalid local message must never put a wrapped 16-bit length onto a
// connection that also carries other callers' valid DNS queries.
func TestDNSClientSharedReviewOversizeKeepsStreamUsable(t *testing.T) {
	pool := newSharedDNSPool(false)
	t.Cleanup(pool.close)
	var dials, queries atomic.Int32
	dial := func(context.Context) (net.Conn, error) {
		dials.Add(1)
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			stream := &D.Conn{Conn: server}
			for {
				query, err := stream.ReadMsg()
				if err != nil {
					return
				}
				queries.Add(1)
				if err := stream.WriteMsg(new(D.Msg).SetReply(query)); err != nil {
					return
				}
			}
		}()
		return client, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	exchange := func(id uint16) {
		t.Helper()
		query := new(D.Msg).SetQuestion("frame-review.example.", D.TypeA)
		query.Id = id
		answer, err := pool.exchange(ctx, query, "same-route", dial)
		if err != nil || answer == nil || answer.Id != id {
			t.Fatalf("valid query could not use the shared stream: answer=%v err=%v", answer, err)
		}
	}
	exchange(41)
	oversize := new(D.Msg).SetQuestion("oversize-review.example.", D.TypeA)
	for i := 0; i < 260; i++ {
		oversize.Extra = append(oversize.Extra, &D.TXT{
			Hdr: D.RR_Header{Name: ".", Rrtype: D.TypeTXT, Class: D.ClassINET},
			Txt: []string{strings.Repeat("x", 255)},
		})
	}
	wire, err := oversize.Pack()
	if err != nil || len(wire) <= dnsmessage.MaxSize {
		t.Fatalf("oversize fixture did not pack above the DNS frame limit: bytes=%d err=%v", len(wire), err)
	}
	if _, err := pool.exchange(ctx, oversize, "same-route", dial); err == nil {
		t.Fatal("oversize DNS message was accepted")
	} else if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("oversize message waited on network I/O instead of being rejected: %v", err)
	}
	exchange(42)
	if dials.Load() != 1 || queries.Load() != 2 {
		t.Fatalf("invalid message changed the shared stream: dials=%d valid queries=%d", dials.Load(), queries.Load())
	}
}

type sharedDNSReviewReadPacket struct {
	net.PacketConn
	read chan net.Addr
}

func (c *sharedDNSReviewReadPacket) ReadFrom(buffer []byte) (int, net.Addr, error) {
	n, source, err := c.PacketConn.ReadFrom(buffer)
	if err == nil {
		select {
		case c.read <- source:
		default:
		}
	}
	return n, source, err
}

// A bound PacketConn is not a connected UDP socket: its reader must retain and
// check the datagram source itself, including when ID and question both match.
func TestDNSClientSharedReviewBoundUDPDropsWrongSource(t *testing.T) {
	listen := func() *net.UDPConn {
		t.Helper()
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	upstream, spoof, client := listen(), listen(), listen()
	observed := &sharedDNSReviewReadPacket{PacketConn: client, read: make(chan net.Addr, 2)}
	pool := newSharedDNSPool(true)
	t.Cleanup(pool.close)
	query := new(D.Msg).SetQuestion("source-review.example.", D.TypeA)
	query.Id = 431
	type result struct {
		answer *D.Msg
		err    error
	}
	results := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		answer, err := pool.exchange(ctx, query, "bound-route", func(context.Context) (net.Conn, error) {
			return N.NewBindPacketConn(observed, upstream.LocalAddr()), nil
		})
		results <- result{answer, err}
	}()
	if err := upstream.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, dnsmessage.MaxSize)
	n, clientAddress, err := upstream.ReadFrom(buffer)
	if err != nil {
		t.Fatal(err)
	}
	request := new(D.Msg)
	if err := request.Unpack(buffer[:n]); err != nil {
		t.Fatal(err)
	}
	send := func(socket *net.UDPConn, address string) {
		t.Helper()
		response := new(D.Msg).SetReply(request)
		response.Answer = []D.RR{&D.A{
			Hdr: D.RR_Header{Name: request.Question[0].Name, Rrtype: D.TypeA, Class: D.ClassINET, Ttl: 60},
			A:   net.ParseIP(address),
		}}
		wire, err := response.Pack()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := socket.WriteTo(wire, clientAddress); err != nil {
			t.Fatal(err)
		}
	}
	send(spoof, "203.0.113.9")
	// Confirm the unconnected client actually received the spoof first; relying
	// on a connected UDP socket would let the kernel hide a broken source check.
	select {
	case source := <-observed.read:
		if source.String() != spoof.LocalAddr().String() {
			t.Fatalf("first received packet was not the spoof: %v", source)
		}
	case <-ctx.Done():
		t.Fatal("the unconnected socket did not receive the spoofed packet")
	}
	send(upstream, "192.0.2.9")
	select {
	case got := <-results:
		if got.err != nil || got.answer == nil || got.answer.Id != query.Id || len(got.answer.Answer) != 1 {
			t.Fatalf("valid upstream response failed: answer=%v err=%v", got.answer, got.err)
		}
		answer, ok := got.answer.Answer[0].(*D.A)
		if !ok || !answer.A.Equal(net.ParseIP("192.0.2.9")) {
			t.Fatalf("matching ID/question from another UDP source satisfied the request: %v", got.answer)
		}
	case <-ctx.Done():
		t.Fatal("valid upstream response was not delivered after rejecting the spoof")
	}
}

type sharedDNSReviewCloseConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *sharedDNSReviewCloseConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

func TestDNSClientSharedReviewCloseDisposesLateDial(t *testing.T) {
	pool := newSharedDNSPool(false)
	t.Cleanup(pool.close)
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	tracked := &sharedDNSReviewCloseConn{Conn: client, closed: make(chan struct{})}
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var dials atomic.Int32
	dial := func(context.Context) (net.Conn, error) {
		dials.Add(1)
		close(started)
		<-release // deliberately emulate a dialer that returns after cancellation
		return tracked, nil
	}
	query := new(D.Msg).SetQuestion("late-dial-review.example.", D.TypeA)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := pool.exchange(ctx, query, "late-route", dial); result <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("dial did not start")
	}
	pool.close()
	select {
	case err := <-result:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("pool closure did not release the query: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("pool closure waited for the uncooperative dialer")
	}
	unblock()
	select {
	case <-tracked.closed:
	case <-ctx.Done():
		t.Fatal("connection returned after pool closure was leaked")
	}
	if _, err := server.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("late connection's peer remained open: %v", err)
	}
	if _, err := pool.exchange(ctx, query, "late-route", dial); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed pool accepted another query: %v", err)
	}
	if dials.Load() != 1 {
		t.Fatalf("closed pool restarted dialing: %d", dials.Load())
	}
}

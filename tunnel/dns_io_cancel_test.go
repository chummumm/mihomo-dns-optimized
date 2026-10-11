package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	N "github.com/metacubex/mihomo/common/net"
	C "github.com/metacubex/mihomo/constant"
)

type dnsCancelTestConn struct {
	net.Conn
	before func() error
	after  func(int)
}

func (c *dnsCancelTestConn) Read(p []byte) (int, error) {
	if c.before != nil {
		if err := c.before(); err != nil {
			return 0, err
		}
	}
	n, err := c.Conn.Read(p)
	if c.after != nil {
		c.after(n)
	}
	return n, err
}

type dnsCancelTestPacket struct {
	net.PacketConn
	before func() error
	after  func(int)
}

func (c *dnsCancelTestPacket) ReadFrom(p []byte) (int, net.Addr, error) {
	if c.before != nil {
		if err := c.before(); err != nil {
			return 0, nil, err
		}
	}
	n, addr, err := c.PacketConn.ReadFrom(p)
	if c.after != nil {
		c.after(n)
	}
	return n, addr, err
}

// Exercise the production framing/packet I/O against real loopback sockets.
// The result race cases cancel immediately after bytes or a native error have
// been obtained, before exchangeDNSProxyWire can return to its observer.
func TestDNSProxyIOCancellationCauseAndLateResults(t *testing.T) {
	for _, network := range []C.NetWork{C.TCP, C.UDP} {
		for _, mode := range []string{"cancel", "deadline", "late-success", "late-eof", "late-reset"} {
			t.Run(network.String()+"/"+mode, func(t *testing.T) {
				query := dnsProxyTestQuery(t, "io-cause.example.")
				response := dnsProxyTestReply(t, query)
				timeout := 2 * time.Second
				if mode == "deadline" {
					timeout = 250 * time.Millisecond
				}
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				ready, release, serverDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
				serverRead := make(chan error, 1)
				var endpoint string
				if network == C.TCP {
					listener, err := net.Listen("tcp4", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					endpoint = listener.Addr().String()
					t.Cleanup(func() { close(release); _ = listener.Close(); <-serverDone })
					go func() {
						defer close(serverDone)
						conn, err := listener.Accept()
						if err != nil {
							serverRead <- err
							close(ready)
							return
						}
						defer conn.Close()
						var size [2]byte
						_, err = io.ReadFull(conn, size[:])
						if err == nil {
							wire := make([]byte, binary.BigEndian.Uint16(size[:]))
							_, err = io.ReadFull(conn, wire)
							if err == nil && !bytes.Equal(wire, query) {
								err = errors.New("query changed")
							}
						}
						serverRead <- err
						close(ready)
						if err == nil && mode == "late-success" {
							binary.BigEndian.PutUint16(size[:], uint16(len(response)))
							_ = writeDNSProxyFrame(conn, append(size[:], response...))
						}
						<-release
					}()
				} else {
					listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					endpoint = listener.LocalAddr().String()
					t.Cleanup(func() { close(release); _ = listener.Close(); <-serverDone })
					go func() {
						defer close(serverDone)
						buf := make([]byte, 4096)
						n, addr, err := listener.ReadFrom(buf)
						if err == nil && !bytes.Equal(buf[:n], query) {
							err = errors.New("query changed")
						}
						serverRead <- err
						close(ready)
						if err == nil && mode == "late-success" {
							_, _ = listener.WriteTo(response, addr)
						}
						<-release
					}()
				}
				var read int
				before := func() error {
					if mode == "late-eof" || mode == "late-reset" {
						<-ready
						cancel()
						if mode == "late-eof" {
							return io.EOF
						}
						return syscall.ECONNRESET
					}
					return nil
				}
				after := func(n int) {
					read += n
					want := len(response)
					if network == C.TCP {
						want += 2
					}
					if mode == "late-success" && read >= want {
						cancel()
					}
				}
				leaf := &dnsProxyTestAdapter{dnsProxyTestBase: newDNSProxyTestBase("io-leaf", C.Direct, true)}
				leaf.dial = func(ctx context.Context, md *C.Metadata) (C.Conn, error) {
					conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", md.RemoteAddress())
					if err != nil {
						return nil, err
					}
					return &dnsProxyTestConn{ExtendedConn: N.NewExtendedConn(&dnsCancelTestConn{Conn: conn, before: before, after: after})}, nil
				}
				leaf.packet = func(ctx context.Context, md *C.Metadata) (C.PacketConn, error) {
					conn, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp4", "127.0.0.1:0")
					if err != nil {
						return nil, err
					}
					return &dnsProxyTestProxyPacketConn{EnhancePacketConn: N.NewEnhancePacketConn(&dnsCancelTestPacket{PacketConn: conn, before: before, after: after})}, nil
				}
				md := dnsProxyTestResolver(network)
				target := netip.MustParseAddrPort(endpoint)
				md.DstIP, md.DstPort = target.Addr(), target.Port()
				type result struct {
					wire []byte
					err  error
				}
				done := make(chan result, 1)
				go func() {
					wire, err := exchangeDNSProxyWire(ctx, query, md, dnsProxyRoute{proxy: leaf})
					done <- result{wire, err}
				}()
				select {
				case err := <-serverRead:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("loopback upstream never received query")
				}
				if mode == "cancel" {
					cancel()
				}
				select {
				case got := <-done:
					switch mode {
					case "cancel":
						if !errors.Is(got.err, context.Canceled) {
							t.Fatalf("canceled I/O lost cause: %v", got.err)
						}
					case "deadline":
						var timeout net.Error
						if !errors.Is(got.err, context.DeadlineExceeded) && !(errors.As(got.err, &timeout) && timeout.Timeout()) {
							t.Fatalf("deadline lost: %v", got.err)
						}
					case "late-success":
						if got.err != nil || !bytes.Equal(got.wire, response) {
							t.Fatalf("late cancel replaced completed answer: %x %v", got.wire, got.err)
						}
					case "late-eof":
						if !errors.Is(got.err, io.EOF) || errors.Is(got.err, context.Canceled) {
							t.Fatalf("late cancel replaced EOF: %v", got.err)
						}
					case "late-reset":
						if !errors.Is(got.err, syscall.ECONNRESET) || errors.Is(got.err, context.Canceled) {
							t.Fatalf("late cancel replaced native reset: %v", got.err)
						}
					}
				case <-time.After(time.Second):
					t.Fatal("I/O failed to finish")
				}
			})
		}
	}
}

package socks5

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type testAddressedConn struct{ net.Conn }

func (testAddressedConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7898}
}

func TestServerHandshakeRequestHandlerRejectsBeforeSuccess(t *testing.T) {
	for _, reject := range []bool{true, false} {
		name := "original-compatible"
		if reject {
			name = "handler-rejection"
		}
		t.Run(name, func(t *testing.T) {
			server, client := net.Pipe()
			defer server.Close()
			defer client.Close()
			_ = client.SetDeadline(time.Now().Add(time.Second))
			result := make(chan error, 1)
			go func() {
				var handler func(Addr, Command, string) (net.Addr, error)
				if reject {
					handler = func(addr Addr, cmd Command, user string) (net.Addr, error) {
						if addr.String() != "8.8.8.8:443" || cmd != CmdConnect || user != "" {
							return nil, ErrGeneralFailure
						}
						return nil, ErrConnectionNotAllowed
					}
				}
				var err error
				if reject {
					_, _, _, err = ServerHandshakeWithHandler(testAddressedConn{server}, nil, handler)
				} else {
					_, _, _, err = ServerHandshake(testAddressedConn{server}, nil)
				}
				result <- err
			}()
			if _, err := client.Write([]byte{Version, 1, 0}); err != nil {
				t.Fatal(err)
			}
			var negotiation [2]byte
			if _, err := io.ReadFull(client, negotiation[:]); err != nil {
				t.Fatal(err)
			}
			if negotiation != [2]byte{Version, 0} {
				t.Fatalf("unexpected negotiation: %v", negotiation)
			}
			if _, err := client.Write(append([]byte{Version, CmdConnect, 0}, ParseAddr("8.8.8.8:443")...)); err != nil {
				t.Fatal(err)
			}
			var reply [10]byte
			if _, err := io.ReadFull(client, reply[:]); err != nil {
				t.Fatal(err)
			}
			err := <-result
			if reject {
				if reply[1] != byte(ErrConnectionNotAllowed) || !errors.Is(err, ErrConnectionNotAllowed) {
					t.Fatalf("rejection must precede success: reply=%v err=%v", reply, err)
				}
			} else if reply[1] != 0 || err != nil {
				t.Fatalf("original handshake changed: reply=%v err=%v", reply, err)
			}
		})
	}
}

package socks4

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestServerHandshakeRequestHandlerRejectsBeforeSuccess(t *testing.T) {
	for _, reject := range []bool{false, true} {
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
				var err error
				if reject {
					_, _, _, err = ServerHandshakeWithHandler(server, nil, func(addr string, command Command, user string) error {
						if addr != "8.8.8.8:443" || command != CmdConnect || user != "user" {
							t.Errorf("incorrect request: %s %d %s", addr, command, user)
						}
						return ErrRequestRejected
					})
				} else {
					_, _, _, err = ServerHandshake(server, nil)
				}
				result <- err
			}()
			if _, err := client.Write([]byte{4, 1, 1, 187, 8, 8, 8, 8, 'u', 's', 'e', 'r', 0}); err != nil {
				t.Fatal(err)
			}
			var reply [8]byte
			if _, err := io.ReadFull(client, reply[:]); err != nil {
				t.Fatal(err)
			}
			err := <-result
			if reject {
				if reply[1] != RequestRejected || err != ErrRequestRejected {
					t.Fatalf("restricted request received success: %v %v", reply, err)
				}
			} else if reply[1] != RequestGranted || err != nil {
				t.Fatalf("original handshake behavior changed: %v %v", reply, err)
			}
		})
	}
}

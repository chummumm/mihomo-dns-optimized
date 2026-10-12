//go:build linux

package redir

import "testing"

func TestManagedRedirReportsSocketFamiliesAndClose(t *testing.T) {
	listener, err := NewManaged(":0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ipv4, _ := listener.TransparentFamilies()
	if !ipv4 {
		t.Fatal("wildcard managed REDIRECT socket has no IPv4 coverage")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if ipv4, ipv6 := listener.TransparentFamilies(); ipv4 || ipv6 {
		t.Fatal("closed listener still reported readiness")
	}
}

func TestLoopbackRedirIsNotGatewayReady(t *testing.T) {
	listener, err := New("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if ipv4, ipv6 := listener.TransparentFamilies(); ipv4 || ipv6 {
		t.Fatal("loopback-only REDIRECT listener reported gateway readiness")
	}
}

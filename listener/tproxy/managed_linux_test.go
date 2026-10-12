//go:build linux

package tproxy

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestManagedTCPWildcardAcceptsActualSocketFamilies(t *testing.T) {
	l, err := listenManagedTCP(net.ListenConfig{}, ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	tcp := l.(*net.TCPListener)
	raw, err := tcp.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	v4, v6 := transparentFamilies(tcp.Addr(), raw)
	if !v4 {
		t.Fatal("managed wildcard socket cannot receive IPv4 destinations")
	}
	_, port, _ := net.SplitHostPort(tcp.Addr().String())
	for _, target := range []struct {
		network, host string
		enabled       bool
	}{
		{"tcp4", "127.0.0.1", v4}, {"tcp6", "::1", v6},
	} {
		if !target.enabled {
			continue
		}
		client, err := net.DialTimeout(target.network, net.JoinHostPort(target.host, port), time.Second)
		if err != nil {
			t.Fatalf("reported %s readiness cannot connect: %v", target.network, err)
		}
		client.Close()
		tcp.SetDeadline(time.Now().Add(time.Second))
		accepted, err := tcp.Accept()
		if err != nil {
			t.Fatal(err)
		}
		accepted.Close()
	}
}

func TestTProxyFamilyReadinessRejectsLoopbackOnlyAndHonorsV6Only(t *testing.T) {
	lc := net.ListenConfig{}
	loopback, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer loopback.Close()
	raw, err := loopback.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if v4, v6 := transparentFamilies(loopback.Addr(), raw); v4 || v6 {
		t.Fatal("loopback listener reported wildcard TProxy readiness")
	}

	onlyV6, err := lc.Listen(context.Background(), "tcp6", "[::]:0")
	if err != nil {
		t.Skipf("IPv6 stack unavailable: %v", err)
	}
	defer onlyV6.Close()
	raw, err = onlyV6.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if v4, v6 := transparentFamilies(onlyV6.Addr(), raw); v4 || !v6 {
		t.Fatalf("IPV6_V6ONLY was ignored: IPv4=%v IPv6=%v", v4, v6)
	}
}

func TestManagedUDPWildcardReportsActualFamilies(t *testing.T) {
	packet, err := listenManagedUDP(":0")
	if err != nil {
		t.Fatal(err)
	}
	defer packet.Close()
	raw, err := packet.(*net.UDPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	v4, v6 := transparentFamilies(packet.LocalAddr(), raw)
	if !v4 {
		t.Fatal("managed UDP socket cannot receive IPv4")
	}
	if packet.LocalAddr().(*net.UDPAddr).IP.To4() == nil && !v6 {
		t.Fatal("wildcard IPv6 UDP socket was not ready")
	}
}

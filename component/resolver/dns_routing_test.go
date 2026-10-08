package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	D "github.com/miekg/dns"
)

type routingDropService struct{}

func (routingDropService) ServeMsg(_ context.Context, query *D.Msg) (*D.Msg, error) {
	switch query.Question[0].Name {
	case "drop.example.":
		return nil, fmt.Errorf("selected policy: %w", ErrDNSDrop)
	case "failed.example.":
		return nil, errors.New("upstream failed")
	default:
		return new(D.Msg).SetReply(query), nil
	}
}

func routingDropQuery(name string, id uint16) *D.Msg {
	query := new(D.Msg).SetQuestion(name, D.TypeA)
	query.Id = id
	return query
}

func TestDNSRuleRoutingRelayUDPDropHasNoServfail(t *testing.T) {
	old := DefaultService
	DefaultService = routingDropService{}
	t.Cleanup(func() { DefaultService = old })
	for _, name := range []string{"drop.example.", "failed.example."} {
		wire, err := routingDropQuery(name, 1).Pack()
		if err != nil {
			t.Fatal(err)
		}
		response, err := RelayDnsPacket(context.Background(), wire, make([]byte, SafeDnsPacketSize))
		if name == "drop.example." {
			if !errors.Is(err, ErrDNSDrop) || len(response) != 0 {
				t.Fatalf("drop became a DNS response: %x %v", response, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		msg := new(D.Msg)
		if err := msg.Unpack(response); err != nil || msg.Rcode != D.RcodeServerFailure {
			t.Fatalf("ordinary upstream failure no longer returns SERVFAIL: %+v %v", msg, err)
		}
	}
}

func TestDNSRuleRoutingRelayTCPDropKeepsNextQuery(t *testing.T) {
	old := DefaultService
	DefaultService = routingDropService{}
	t.Cleanup(func() { DefaultService = old })
	client, server := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan error, 1)
	go func() { done <- RelayDnsConn(context.Background(), server, time.Second) }()
	conn := &D.Conn{Conn: client}
	if err := conn.WriteMsg(routingDropQuery("drop.example.", 10)); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMsg(routingDropQuery("allowed.example.", 11)); err != nil {
		t.Fatal(err)
	}
	response, err := conn.ReadMsg()
	if err != nil || response.Id != 11 || response.Rcode != D.RcodeSuccess {
		t.Fatalf("drop generated a response or closed later queries: %+v %v", response, err)
	}
	_ = client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DNS relay failed to close")
	}
}

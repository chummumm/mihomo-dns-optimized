package dns

import (
	"context"
	"reflect"
	"testing"

	D "github.com/miekg/dns"
)

func TestSpeedCheckUnmeasuredFallbackUsesIndependentAgreement(t *testing.T) {
	query := new(D.Msg).SetQuestion("fallback-vote.example.", D.TypeA)
	first := selectionTestAnswer(query, "cdn.example.", "192.0.2.1", "192.0.2.2")
	repeated := selectionTestAnswer(query, "cdn.example.", "192.0.2.2", "192.0.2.3")
	third := selectionTestAnswer(query, "cdn.example.", "192.0.2.2")
	answer := speedCheckUnmeasuredAnswer(context.Background(), first, query.Question[0], []*D.Msg{first, repeated, third}, 8)
	if ips := msgToIP(answer); len(ips) != 1 || ips[0].String() != "192.0.2.2" || len(answer.Answer) != 2 {
		t.Fatalf("no-measurement consensus lost address or intact chain: %v", answer)
	}
}

func TestSpeedCheckUnmeasuredFallbackDoesNotCountDuplicateRRsOrCrossChains(t *testing.T) {
	query := new(D.Msg).SetQuestion("fallback-vote.example.", D.TypeA)
	first := selectionTestAnswer(query, "first.example.", "192.0.2.1", "192.0.2.2")
	duplicate := selectionTestAnswer(query, "other.example.", "192.0.2.2", "192.0.2.2", "192.0.2.2")
	before := first.String()
	answer := speedCheckUnmeasuredAnswer(context.Background(), first, query.Question[0], []*D.Msg{first, duplicate}, 8)
	if !reflect.DeepEqual(msgToIP(answer), msgToIP(first)) || first.String() != before {
		t.Fatalf("duplicate or incompatible records fabricated agreement: %v", answer)
	}
}

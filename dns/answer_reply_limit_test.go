package dns

import (
	"context"
	"fmt"
	"testing"
	"time"

	D "github.com/miekg/dns"
)

func TestDNSOptimizationReplyIPLimitWithoutSpeedCheck(t *testing.T) {
	for _, qtype := range []uint16{D.TypeA, D.TypeAAAA} {
		for _, limit := range []int{0, 1, 3, 32} {
			t.Run(fmt.Sprintf("type=%d/limit=%d", qtype, limit), func(t *testing.T) {
				query := new(D.Msg).SetQuestion("reply-limit.example.", qtype)
				var addresses []string
				for i := 1; i <= 12; i++ {
					address := fmt.Sprintf("192.0.2.%d", i)
					if qtype == D.TypeAAAA {
						address = fmt.Sprintf("2001:db8::%x", i)
					}
					addresses = append(addresses, address)
				}
				client := &dualStackTestClient{exchange: func(_ context.Context, q *D.Msg) (*D.Msg, error) {
					return selectionTestAnswer(q, "cdn.example.", addresses...), nil
				}}
				r := NewResolverFromClient(client)
				r.answerPolicy = AnswerPolicy{ReplyIPLimit: limit}
				t.Cleanup(r.Close)
				want := limit
				if want == 0 || want > len(addresses) {
					want = len(addresses)
				}
				for i := 0; i < 2; i++ {
					query.Id++
					answer, err := r.ExchangeContext(context.Background(), query)
					if err != nil || len(msgToIP(answer)) != want || answer.Answer[0].Header().Rrtype != D.TypeCNAME {
						t.Fatalf("ordinary answer limit/chain changed: want=%d answer=%v err=%v", want, answer, err)
					}
				}
				if r.speedChecker != nil || client.count() != 1 {
					t.Fatal("reply limit enabled probes or bypassed the ordinary cache")
				}
			})
		}
	}
}

func TestDNSOptimizationReplyIPLimitProtectedAnswers(t *testing.T) {
	for _, protection := range []string{"DO", "CD", "response-AD", "TSIG", "CNAME-question", "query-AD"} {
		t.Run(protection, func(t *testing.T) {
			query := new(D.Msg).SetQuestion("limit.example.", D.TypeA)
			switch protection {
			case "DO":
				query.SetEdns0(1232, true)
			case "CD":
				query.CheckingDisabled = true
			case "TSIG":
				query.SetTsig("key.example.", D.HmacSHA256, 300, time.Now().Unix())
			case "CNAME-question":
				query.Question[0].Qtype = D.TypeCNAME
			case "query-AD":
				query.AuthenticatedData = true
			}
			answer := selectionTestAnswer(query, "", "192.0.2.1", "192.0.2.2")
			if protection == "response-AD" {
				answer.AuthenticatedData = true
			}
			before := answer.String()
			got := (AnswerPolicy{ReplyIPLimit: 1}).apply(query, answer)
			want := 2
			if protection == "query-AD" {
				want = 1
			}
			if len(msgToIP(got)) != want || answer.String() != before {
				t.Fatalf("protection or snapshot immutability changed: %v", got)
			}
		})
	}
}

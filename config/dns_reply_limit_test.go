package config

import "testing"

func TestDNSOptimizationExplicitReplyLimitPreservesDefaults(t *testing.T) {
	for _, fields := range []string{"", "  max-reply-ip-num: 2\n", "  speed-check-mode: [none]\n  max-reply-ip-num: 3\n"} {
		raw, err := UnmarshalRawConfig([]byte("dns:\n  nameserver: [192.0.2.53]\n" + fields))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parseDNS(raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if raw.DNS.MaxReplyIPNum != nil {
			want = int(*raw.DNS.MaxReplyIPNum)
		}
		if parsed.AnswerPolicy.ReplyIPLimit != want {
			t.Fatalf("explicit-only reply limit lost: got=%d want=%d", parsed.AnswerPolicy.ReplyIPLimit, want)
		}
		if len(parsed.SpeedCheck.Mode) == 0 && fields != "" && parsed.CacheOptions.Prefetch {
			t.Fatal("reply limit enabled unrelated tuning")
		}
	}
}

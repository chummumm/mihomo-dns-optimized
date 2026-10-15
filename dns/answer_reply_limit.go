package dns

import D "github.com/miekg/dns"

// An explicit reply limit applies independently of optional speed checks.
// The zero value leaves ordinary Mihomo replies intact. Only the terminal
// address RRset is limited; aliases and unrelated answer records keep their
// identity, and authenticated or DNSSEC-sensitive answers are not rewritten.
func limitDNSAddressReply(query, message *D.Msg, limit int) *D.Msg {
	if limit <= 0 || !speedCheckAddressQuestion(query) || !answerPolicyCanRewrite(query, message) {
		return message
	}
	return speedCheckLimitAnswer(message, query.Question[0], limit)
}

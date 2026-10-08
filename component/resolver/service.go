package resolver

import (
	"context"
	"errors"

	D "github.com/miekg/dns"
)

var DefaultService Service

// ErrDNSDrop is an intentional DNS policy action. Local DNS serving paths must
// not translate it into SERVFAIL or any other response.
var ErrDNSDrop = errors.New("DNS rule rejected this query without a response")

type Service interface {
	ServeMsg(ctx context.Context, msg *D.Msg) (*D.Msg, error)
}

// ServeMsg with a dns.Msg, return resolve dns.Msg
func ServeMsg(ctx context.Context, msg *D.Msg) (*D.Msg, error) {
	if server := DefaultService; server != nil {
		return server.ServeMsg(ctx, msg)
	}

	return nil, ErrIPNotFound
}

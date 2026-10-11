package dns

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"sync"

	"github.com/metacubex/mihomo/common/deque"
	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/component/ca"
	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"

	"github.com/metacubex/tls"
	D "github.com/miekg/dns"
)

const maxOldDotConns = 8

type dnsOverTLS struct {
	native         nativeClientState
	port           string
	host           string
	dialer         *dnsDialer
	skipCertVerify bool
	nameCertVerify string
	disableReuse   bool

	access      sync.Mutex
	connections deque.Deque[net.Conn] // LIFO
}

var _ dnsClient = (*dnsOverTLS)(nil)

// Address implements dnsClient
func (t *dnsOverTLS) Address() string {
	return fmt.Sprintf("tls://%s", net.JoinHostPort(t.host, t.port))
}

func (t *dnsOverTLS) ExchangeContext(ctx context.Context, m *D.Msg) (response *D.Msg, err error) {
	if answer, err, handled := exchangeNativeTransport(ctx, m, t); handled {
		return answer, err
	}
	attempt := observeDNSUpstream(ctx, t.Address(), t.dialer)
	defer func() { finishDNSUpstream(attempt, response, err) }()
	for { // retry loop; only retry when reusing old conn
		if ctxErr := ctx.Err(); ctxErr != nil {
			// A reused socket may already have failed before cancellation.
			// If its retry has not started, keep that actual failure.
			if err != nil {
				return response, err
			}
			return nil, ctxErr
		}

		var conn net.Conn
		isOldConn := true
		if !t.disableReuse {
			t.access.Lock()
			if t.connections.Len() > 0 {
				conn = t.connections.PopBack()
			}
			t.access.Unlock()
		}
		if conn == nil {
			conn, err = t.dialContext(ctx)
			if err != nil {
				return nil, err
			}
			isOldConn = false
		}

		response, err = exchangeDNSWithConn(ctx, m, conn)
		if err != nil {
			abortDNSConn(conn)
			if isOldConn && ctx.Err() == nil {
				continue
			}
			return response, err
		}
		// Complete before cleanup or pool-lock contention. A late cancellation
		// can retire the socket but cannot replace this completed DNS response.
		finishDNSUpstream(attempt, response, nil)
		if !t.disableReuse {
			t.access.Lock()
			if t.native.isClosed() || ctx.Err() != nil {
				t.access.Unlock()
				abortDNSConn(conn)
				return response, nil
			}
			if t.connections.Len() >= maxOldDotConns {
				oldConn := t.connections.PopFront()
				go oldConn.Close() // close in a new goroutine, not blocking the current task
			}
			t.connections.PushBack(conn)
			t.access.Unlock()
		} else {
			if t.native.isClosed() || ctx.Err() != nil {
				abortDNSConn(conn)
			} else {
				// Normal non-reuse keeps a graceful TLS close. If cancellation
				// arrives after Exchange stopped its I/O guard, interrupt this
				// cleanup too, without changing the completed response.
				guard := N.NewDNSCancelGuard(ctx, func() { abortDNSConn(conn) })
				_ = conn.Close()
				guard.Stop()
			}
		}
		return response, nil
	}
}

func (t *dnsOverTLS) dialContext(ctx context.Context) (net.Conn, error) {
	conn, err := t.dialer.DialContext(ctx, "tcp", net.JoinHostPort(t.host, t.port))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	tlsConfig, err := ca.GetTLSConfig(ca.Option{
		TLSConfig: &tls.Config{
			ServerName:         t.host,
			InsecureSkipVerify: t.skipCertVerify,
		},
		NameCertVerify: t.nameCertVerify,
	})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	tlsConn := tls.Client(conn, tlsConfig)
	if err = tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	conn = tlsConn

	return conn, nil
}

func (t *dnsOverTLS) ResetConnection() {
	t.native.reset(false)
	if !t.disableReuse {
		t.access.Lock()
		for t.connections.Len() > 0 {
			oldConn := t.connections.PopFront()
			go oldConn.Close() // close in a new goroutine, not blocking the current task
		}
		t.access.Unlock()
	}
}

func (t *dnsOverTLS) Close() error {
	t.native.reset(true)
	runtime.SetFinalizer(t, nil)
	t.ResetConnection()
	return nil
}

func newDoTClient(addr string, resolver resolver.Resolver, params map[string]string, proxyAdapter C.ProxyAdapter, proxyName string) *dnsOverTLS {
	host, port, _ := net.SplitHostPort(addr)
	c := &dnsOverTLS{
		port:   port,
		host:   host,
		dialer: newDNSDialer(resolver, proxyAdapter, proxyName),
	}
	c.connections.SetBaseCap(maxOldDotConns)
	if params["skip-cert-verify"] == "true" {
		c.skipCertVerify = true
	}
	c.nameCertVerify = params["name-cert-verify"]
	if params["disable-reuse"] == "true" {
		c.disableReuse = true
	}
	runtime.SetFinalizer(c, (*dnsOverTLS).Close)
	return c
}

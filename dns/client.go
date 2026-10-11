package dns

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/log"

	D "github.com/miekg/dns"
)

type client struct {
	native       nativeClientState
	port         string
	host         string
	dialer       *dnsDialer
	schema       string
	resolver     resolver.Resolver
	proxyAdapter C.ProxyAdapter
	proxyName    string
}

var _ dnsClient = (*client)(nil)

// Address implements dnsClient
func (c *client) Address() string {
	return fmt.Sprintf("%s://%s", c.schema, net.JoinHostPort(c.host, c.port))
}

func (c *client) ExchangeContext(ctx context.Context, m *D.Msg) (response *D.Msg, err error) {
	if answer, err, handled := exchangeNativeTransport(ctx, m, c); handled {
		return answer, err
	}
	if route := queryRoute(ctx); route != nil && !c.native.bound && !icontext.DNSBootstrap(ctx) && c.canRouteDNS() {
		return c.exchangeRouted(ctx, m, route)
	}
	attempt := observeDNSUpstream(ctx, c.Address(), c.dialer)
	defer func() { finishDNSUpstream(attempt, response, err) }()
	network := "udp"
	if c.schema != "udp" {
		network = "tcp"
	}

	addr := net.JoinHostPort(c.host, c.port)
	conn, err := c.dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	response, err = exchangeDNSWithConn(ctx, m, conn)
	// Resolvers MUST resend truncated UDP replies over TCP. The fallback
	// owns a separate cancellation guard, so cancel closes the active socket.
	if err == nil && response != nil && response.Truncated && network == "udp" {
		log.Debugln("[DNS] Truncated reply from %s:%s for %s over UDP, retrying over TCP", c.host, c.port, m.Question[0].String())
		tcpConn, dialErr := c.dialer.DialContext(ctx, "tcp", addr)
		if dialErr != nil {
			return response, dialErr
		}
		defer tcpConn.Close()
		response, err = exchangeDNSWithConn(ctx, m, tcpConn)
	}
	finishDNSUpstream(attempt, response, err)
	return response, err
}

// A DNS exchange owns this connection until it returns. Interrupt that socket
// on cancellation and observe the synchronous I/O result: racing ctx.Done with
// a worker's result channel can discard a reply/EOF already obtained by I/O.
// The caller retains ownership, including normal close or DoT pool return.
func exchangeDNSWithConn(ctx context.Context, message *D.Msg, conn net.Conn) (*D.Msg, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	guard := N.NewDNSCancelGuard(ctx, func() { abortDNSConn(conn) })
	defer guard.Stop()
	client := &D.Client{UDPSize: 4096, Timeout: 5 * time.Second}
	response, _, err := client.ExchangeWithConnContext(ctx, message, &D.Conn{Conn: conn, UDPSize: client.UDPSize})
	return response, guard.Error(err, N.DNSClosedIO(err))
}

// Interrupt only an exchange-owned socket. TLS Close writes close_notify with
// its own five-second timeout; cancellation must close its transport directly
// so a peer which stopped reading cannot hold the DNS exchange open.
func abortDNSConn(conn net.Conn) {
	if tlsConn, ok := conn.(interface{ NetConn() net.Conn }); ok {
		conn = tlsConn.NetConn()
	}
	_ = conn.Close()
}

func (c *client) ResetConnection() { c.native.reset(false) }
func (c *client) Close() error     { c.native.reset(true); return nil }

func newClient(addr string, resolver resolver.Resolver, netType string, params map[string]string, proxyAdapter C.ProxyAdapter, proxyName string) *client {
	host, port, _ := net.SplitHostPort(addr)
	c := &client{
		port:         port,
		host:         host,
		dialer:       newDNSDialer(resolver, proxyAdapter, proxyName),
		schema:       "udp",
		resolver:     resolver,
		proxyAdapter: proxyAdapter,
		proxyName:    proxyName,
	}
	if strings.HasPrefix(netType, "tcp") {
		c.schema = "tcp"
	}
	return c
}

package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/tunnel"
	D "github.com/miekg/dns"
)

const nativeTransportCapacity = 64

// Every configured upstream keeps a legacy client and a bounded set of private
// protocol clients. Only immutable connection-routing identity keys this pool;
// query/source identities stay in the logical query cache and tracker.
type nativeClientState struct {
	bound  bool
	mu     sync.Mutex
	pool   *nativeTransportPool
	closed bool
}

type nativeTransportEntry struct {
	client dnsClient
	users  int
	last   uint64
}

type nativeTransportPool struct {
	mu       sync.Mutex
	entries  map[string]*nativeTransportEntry
	capacity int
	clock    uint64
	changed  chan struct{}
	lifetime context.Context
	cancel   context.CancelFunc
	closed   bool
}

func newNativeTransportPool(capacity int) *nativeTransportPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &nativeTransportPool{entries: make(map[string]*nativeTransportEntry), capacity: capacity,
		changed: make(chan struct{}), lifetime: ctx, cancel: cancel}
}

func (p *nativeTransportPool) signalLocked() { close(p.changed); p.changed = make(chan struct{}) }

func closeNativeClient(client dnsClient) {
	if closer, ok := client.(interface{ Close() error }); ok {
		_ = closer.Close()
	} else {
		client.ResetConnection()
	}
}

func (p *nativeTransportPool) acquire(ctx context.Context, key string, create func() dnsClient) (*nativeTransportEntry, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, net.ErrClosed
		}
		p.clock++
		if entry := p.entries[key]; entry != nil {
			entry.users++
			entry.last = p.clock
			p.mu.Unlock()
			return entry, nil
		}
		if len(p.entries) < p.capacity {
			entry := &nativeTransportEntry{client: create(), users: 1, last: p.clock}
			p.entries[key] = entry
			p.mu.Unlock()
			return entry, nil
		}
		var oldest *nativeTransportEntry
		var oldestKey string
		for candidateKey, candidate := range p.entries {
			if candidate.users == 0 && (oldest == nil || candidate.last < oldest.last) {
				oldest, oldestKey = candidate, candidateKey
			}
		}
		if oldest != nil {
			delete(p.entries, oldestKey)
			p.mu.Unlock()
			closeNativeClient(oldest.client)
			continue
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (p *nativeTransportPool) release(entry *nativeTransportEntry) {
	p.mu.Lock()
	entry.users--
	p.clock++
	entry.last = p.clock
	p.signalLocked()
	p.mu.Unlock()
}

func (p *nativeTransportPool) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.cancel()
	entries := p.entries
	p.entries = nil
	p.signalLocked()
	p.mu.Unlock()
	// Lifecycle shutdown cancels the associated requests first. LRU eviction
	// above never closes an entry whose siblings are still exchanging queries.
	for _, entry := range entries {
		closeNativeClient(entry.client)
	}
}

func (s *nativeClientState) reset(close bool) {
	s.mu.Lock()
	s.closed = s.closed || close
	pool := s.pool
	s.pool = nil
	s.mu.Unlock()
	if pool != nil {
		pool.close()
	}
}

func (s *nativeClientState) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func closeNativePool(dc dnsClient) {
	if wrapped, ok := dc.(interface{ Unwrap() dnsClient }); ok {
		closeNativePool(wrapped.Unwrap())
		return
	}
	if state, _, _, _ := nativeClientDetails(dc); state != nil {
		state.reset(true)
	}
}

func nativeClientDetails(dc dnsClient) (*nativeClientState, *dnsDialer, C.NetWork, string) {
	switch c := dc.(type) {
	case *client:
		network := C.UDP
		if c.schema == "tcp" {
			network = C.TCP
		}
		return &c.native, c.dialer, network, net.JoinHostPort(c.host, c.port)
	case *dnsOverTLS:
		return &c.native, c.dialer, C.TCP, net.JoinHostPort(c.host, c.port)
	case *dnsOverQUIC:
		return &c.native, c.dialer, C.UDP, c.addr
	case *dnsOverHTTPS:
		port := c.url.Port()
		if port == "" {
			if c.url.Scheme == "http" {
				port = "80"
			} else {
				port = "443"
			}
		}
		network := C.TCP
		if c.supportsH3() && !c.supportsHTTP() {
			network = C.UDP
		}
		return &c.native, c.dialer, network, net.JoinHostPort(c.url.Hostname(), port)
	}
	return nil, nil, C.UDP, ""
}

func cloneNativeClient(dc dnsClient, plan *tunnel.DNSRoutingPlan, lifetime context.Context) dnsClient {
	params := make(map[string]string)
	var clone dnsClient
	switch c := dc.(type) {
	case *client:
		clone = newClient(net.JoinHostPort(c.host, c.port), c.resolver, c.schema, nil, nil, "")
	case *dnsOverTLS:
		params["skip-cert-verify"] = fmt.Sprint(c.skipCertVerify)
		params["name-cert-verify"] = c.nameCertVerify
		params["disable-reuse"] = fmt.Sprint(c.disableReuse)
		clone = newDoTClient(net.JoinHostPort(c.host, c.port), nil, params, nil, "")
	case *dnsOverHTTPS:
		params["skip-cert-verify"] = fmt.Sprint(c.skipCertVerify)
		params["name-cert-verify"] = c.nameCertVerify
		cloned := newDoHClient(c.addr, nil, false, params, nil, "").(*dnsOverHTTPS)
		cloned.httpVersions = append([]C.HTTPVersion(nil), c.httpVersions...)
		clone = cloned
	case *dnsOverQUIC:
		params["skip-cert-verify"] = fmt.Sprint(c.skipCertVerify)
		params["name-cert-verify"] = c.nameCertVerify
		clone = newDoQ(c.addr, nil, params, nil, "")
	default:
		panic("unsupported native DNS client")
	}
	state, _, _, _ := nativeClientDetails(clone)
	state.bound = true
	_, original, _, _ := nativeClientDetails(dc)
	dialer := original.WithPlan(plan, lifetime)
	switch c := clone.(type) {
	case *client:
		c.dialer = dialer
	case *dnsOverTLS:
		c.dialer = dialer
	case *dnsOverHTTPS:
		c.dialer = dialer
	case *dnsOverQUIC:
		c.dialer = dialer
	}
	return clone
}

func exchangeNativeTransport(ctx context.Context, message *D.Msg, dc dnsClient) (*D.Msg, error, bool) {
	state, dialer, network, endpoint := nativeClientDetails(dc)
	if state != nil && state.bound && state.isClosed() {
		return nil, net.ErrClosed, true
	}
	route := queryRoute(ctx)
	if state == nil || state.bound || route == nil || icontext.DNSBootstrap(ctx) || !dialer.Automatic() {
		return nil, nil, false
	}
	// Preserve the original per-datagram/framed tracker and strict wire path
	// for ordinary port 53. Configured native non-53 clients use the dialer.
	if c, ok := dc.(*client); ok && c.port == "53" {
		return nil, nil, false
	}
	if route.err != nil {
		return nil, route.err, true
	}
	if route.plan == nil {
		return nil, errors.New("native DNS has no routing plan"), true
	}
	if kind := route.plan.Type(); kind == C.Reject || kind == C.RejectDrop {
		return nil, errors.New("automatic native DNS route is rejected"), true
	}
	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		return nil, net.ErrClosed, true
	}
	if state.pool == nil {
		state.pool = newNativeTransportPool(nativeTransportCapacity)
	}
	pool := state.pool
	state.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(pool.lifetime, cancel)
	defer stop()
	defer cancel()
	entry, err := pool.acquire(ctx, route.plan.NativeTransportKey(), func() dnsClient { return cloneNativeClient(dc, route.plan, pool.lifetime) })
	if err != nil {
		return nil, err, true
	}
	defer pool.release(entry)
	ctx, tracker, err := route.plan.TrackNativeQuery(ctx, endpoint, network, message.Len())
	if err != nil {
		return nil, err, true
	}
	defer tracker.Close()
	answer, err := entry.client.ExchangeContext(ctx, message)
	if errors.Is(context.Cause(ctx), tunnel.ErrDNSNativeQueryClosed) {
		route.closed.Store(true)
		return nil, tunnel.ErrDNSNativeQueryClosed, true
	}
	if err == nil {
		if answer == nil || !answer.Response || answer.Opcode != message.Opcode || answer.Id != message.Id {
			err = errors.New("native DNS received an unrelated response")
		} else if len(answer.Question) == 0 && answer.Rcode != D.RcodeSuccess && len(answer.Answer)+len(answer.Ns)+len(answer.Extra) == 0 {
			// Header-only error replies are valid, as in the ordinary DNS path.
		} else if len(answer.Question) != 1 || len(message.Question) != 1 ||
			!strings.EqualFold(answer.Question[0].Name, message.Question[0].Name) ||
			answer.Question[0].Qtype != message.Question[0].Qtype || answer.Question[0].Qclass != message.Question[0].Qclass {
			err = errors.New("native DNS received a response for another question")
		}
	}
	if err != nil {
		return nil, err, true
	}
	tracker.DownloadTotal.Add(int64(answer.Len()))
	return answer, nil, true
}

func dnsQueryClosed(ctx context.Context) bool {
	route := queryRoute(ctx)
	return route != nil && route.closed.Load()
}

package dns

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/component/dhcp"
	"github.com/metacubex/mihomo/component/iface"
	D "github.com/miekg/dns"
)

const (
	IfaceTTL    = time.Second * 20
	DHCPTTL     = time.Hour
	DHCPTimeout = time.Minute
)

type dhcpClient struct {
	ifaceName string

	lock            sync.Mutex
	ifaceInvalidate time.Time
	dnsInvalidate   time.Time

	ifaceAddr netip.Prefix
	done      chan struct{}
	clients   []dnsClient
	err       error
	closed    bool
	cancel    context.CancelFunc
}

var _ dnsClient = (*dhcpClient)(nil)

// Address implements dnsClient
func (d *dhcpClient) Address() string {
	d.lock.Lock()
	clients := append([]dnsClient(nil), d.clients...)
	d.lock.Unlock()
	addrs := make([]string, 0)
	for _, c := range clients {
		addrs = append(addrs, c.Address())
	}
	return strings.Join(addrs, ",")
}

func (d *dhcpClient) ExchangeContext(ctx context.Context, m *D.Msg) (msg *D.Msg, err error) {
	clients, err := d.resolve(ctx)
	if err != nil {
		return nil, err
	}

	msg, _, err = batchExchange(ctx, clients, m)
	return
}

func (d *dhcpClient) ResetConnection() {
	d.lock.Lock()
	clients := append([]dnsClient(nil), d.clients...)
	d.lock.Unlock()
	for _, client := range clients {
		client.ResetConnection()
	}
}

func (d *dhcpClient) Close() error {
	d.lock.Lock()
	d.closed = true
	if d.cancel != nil {
		d.cancel()
	}
	clients := d.clients
	d.clients = nil
	d.err = net.ErrClosed
	d.lock.Unlock()
	for _, client := range clients {
		closeNativeClient(client)
	}
	return nil
}

func (d *dhcpClient) resolve(ctx context.Context) ([]dnsClient, error) {
	d.lock.Lock()
	if d.closed {
		d.lock.Unlock()
		return nil, net.ErrClosed
	}

	invalidated, err := d.invalidate()
	if err != nil {
		d.err = err
	} else if invalidated {
		done := make(chan struct{})

		d.done = done
		ctx, cancel := context.WithTimeout(context.Background(), DHCPTimeout)
		d.cancel = cancel

		go func() {
			defer cancel()

			var res []dnsClient
			dns, err := dhcp.ResolveDNSFromDHCP(ctx, d.ifaceName)
			// dns never empty if err is nil
			if err == nil {
				nameserver := make([]NameServer, 0, len(dns))
				for _, item := range dns {
					nameserver = append(nameserver, NameServer{
						Addr:      net.JoinHostPort(item.String(), "53"),
						ProxyName: d.ifaceName,
					})
				}

				res = transform(nameserver, nil)
			}

			d.lock.Lock()
			old := d.clients
			if d.closed {
				old, res, err = res, nil, net.ErrClosed
			}
			close(done)
			d.done = nil
			d.cancel = nil
			d.clients = res
			d.err = err
			d.lock.Unlock()
			for _, client := range old {
				closeNativeClient(client)
			}
		}()
	}

	d.lock.Unlock()

	for {
		d.lock.Lock()

		res, err, done := d.clients, d.err, d.done

		d.lock.Unlock()

		// initializing
		if res == nil && err == nil {
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		// dirty return
		return res, err
	}
}

func (d *dhcpClient) invalidate() (bool, error) {
	if time.Now().Before(d.ifaceInvalidate) {
		return false, nil
	}

	d.ifaceInvalidate = time.Now().Add(IfaceTTL)

	ifaceObj, err := iface.ResolveInterface(d.ifaceName)
	if err != nil {
		return false, err
	}

	addr, err := ifaceObj.PickIPv4Addr(netip.Addr{})
	if err != nil {
		return false, err
	}

	if time.Now().Before(d.dnsInvalidate) && d.ifaceAddr == addr {
		return false, nil
	}

	d.dnsInvalidate = time.Now().Add(DHCPTTL)
	d.ifaceAddr = addr

	return d.done == nil, nil
}

func newDHCPClient(ifaceName string) *dhcpClient {
	return &dhcpClient{ifaceName: ifaceName}
}

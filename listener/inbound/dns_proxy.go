package inbound

import (
	"errors"
	"fmt"
	"strings"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/listener/dnsproxy"
	"github.com/metacubex/mihomo/log"
)

type DNSProxyOption struct {
	BaseOption
	Enable bool      `inbound:"enable,omitempty"`
	Users  AuthUsers `inbound:"users,omitempty"`
}

func (o DNSProxyOption) Equal(config C.InboundConfig) bool {
	return optionToString(o) == optionToString(config)
}

type DNSProxy struct {
	*Base
	config    *DNSProxyOption
	listeners []*dnsproxy.Listener
}

func NewDNSProxy(options *DNSProxyOption) (*DNSProxy, error) {
	if options.SpecialRules != "" || options.SpecialProxy != "" {
		return nil, errors.New("dns-proxy uses global rules; listener rule/proxy overrides are not supported")
	}
	if options.Listen == "" {
		options.Listen = "127.0.0.1"
	}
	base, err := NewBase(&options.BaseOption)
	if err != nil {
		return nil, err
	}
	return &DNSProxy{Base: base, config: options}, nil
}

func (d *DNSProxy) Config() C.InboundConfig { return d.config }

func (d *DNSProxy) Address() string {
	var addresses []string
	for _, l := range d.listeners {
		addresses = append(addresses, l.Address())
	}
	return strings.Join(addresses, ",")
}

func (d *DNSProxy) Listen(tunnel C.Tunnel) error {
	if !d.config.Enable {
		return nil
	}
	if len(d.listeners) != 0 {
		return errors.New("dns-proxy listener is already running")
	}
	exchanger, ok := tunnel.(C.DNSExchanger)
	if !ok {
		return errors.New("dns-proxy requires a tunnel with DNS exchange support")
	}
	lc := d.ListenConfig()
	for _, addr := range strings.Split(d.RawAddress(), ",") {
		l, err := dnsproxy.New(addr, lc, d.config.Users.GetAuthStore(), exchanger, d.Additions()...)
		if err != nil {
			_ = d.Close()
			return err
		}
		d.listeners = append(d.listeners, l)
	}
	log.Infoln("DNS-only SOCKS5/HTTP CONNECT[%s] listening at: %s", d.Name(), d.Address())
	return nil
}

func (d *DNSProxy) Close() error {
	var errs []error
	for _, l := range d.listeners {
		if err := l.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close dns-proxy listener %s: %w", l.Address(), err))
		}
	}
	d.listeners = nil
	return errors.Join(errs...)
}

var _ C.InboundListener = (*DNSProxy)(nil)

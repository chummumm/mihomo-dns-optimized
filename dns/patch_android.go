//go:build android && cmfa

package dns

import (
	"net"
	"sync"

	"github.com/metacubex/mihomo/component/resolver"
)

// Android's system DNS transports belong to this process-wide configuration,
// not to an individual systemClient wrapper. A wrapper Close cannot interrupt
// other resolver collections; UpdateSystemDNS retires the old shared clients.
var systemResolver []dnsClient
var systemResolverMu sync.Mutex

func FlushCacheWithDefaultResolver() {
	resolver.ClearCache()
	resolver.ResetConnection()
}

func UpdateSystemDNS(addr []string) {
	ns := make([]NameServer, 0, len(addr))
	for _, d := range addr {
		ns = append(ns, NameServer{Addr: d})
	}

	clients := transform(ns, nil)
	systemResolverMu.Lock()
	old := systemResolver
	systemResolver = clients
	systemResolverMu.Unlock()
	for _, dc := range old {
		closeNativeClient(dc)
	}
}

func (c *systemClient) getDnsClients() ([]dnsClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, net.ErrClosed
	}
	systemResolverMu.Lock()
	defer systemResolverMu.Unlock()
	return append([]dnsClient(nil), systemResolver...), nil
}

func (c *systemClient) ResetConnection() {
	clients, _ := c.getDnsClients()
	for _, r := range clients {
		r.ResetConnection()
	}
	c.resetClients()
}

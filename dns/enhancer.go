package dns

import (
	"errors"
	"net/netip"
	"strings"

	"github.com/metacubex/mihomo/common/lru"
	"github.com/metacubex/mihomo/component/fakeip"
	"github.com/metacubex/mihomo/component/trie"
	C "github.com/metacubex/mihomo/constant"
)

type ResolverEnhancer struct {
	ipv6            bool
	mode            C.DNSMode
	fakeIPPool      *fakeip.Pool
	fakeIPPool6     *fakeip.Pool
	fakeIPSkipper   *fakeip.Skipper
	fakeIPTTL       int
	mapping         *lru.LruCache[netip.Addr, string]
	redirHostFilter *trie.DomainSet
	useHosts        bool
}

func (h *ResolverEnhancer) FakeIPEnabled() bool {
	return h.mode == C.DNSFakeIP
}

func (h *ResolverEnhancer) MappingEnabled() bool {
	return h.mode == C.DNSFakeIP || h.mode == C.DNSMapping
}

func (h *ResolverEnhancer) IsExistFakeIP(ip netip.Addr) bool {
	if !h.FakeIPEnabled() {
		return false
	}

	if pool := h.fakeIPPool; pool != nil {
		if pool.Exist(ip) {
			return true
		}
	}

	if pool6 := h.fakeIPPool6; pool6 != nil {
		if pool6.Exist(ip) {
			return true
		}
	}

	return false
}

func (h *ResolverEnhancer) IsFakeIP(ip netip.Addr) bool {
	if !h.FakeIPEnabled() {
		return false
	}

	if pool := h.fakeIPPool; pool != nil {
		if pool.IPNet().Contains(ip) && ip != pool.Gateway() && ip != pool.Broadcast() {
			return true
		}
	}

	if pool6 := h.fakeIPPool6; pool6 != nil {
		if pool6.IPNet().Contains(ip) && ip != pool6.Gateway() && ip != pool6.Broadcast() {
			return true
		}
	}

	return false
}

func (h *ResolverEnhancer) IsFakeBroadcastIP(ip netip.Addr) bool {
	if !h.FakeIPEnabled() {
		return false
	}

	if pool := h.fakeIPPool; pool != nil {
		if pool.Broadcast() == ip {
			return true
		}
	}

	if pool6 := h.fakeIPPool6; pool6 != nil {
		if pool6.Broadcast() == ip {
			return true
		}
	}

	return false
}

func (h *ResolverEnhancer) FindHostByIP(ip netip.Addr) (string, bool) {
	if pool := h.fakeIPPool; pool != nil {
		if host, existed := pool.LookBack(ip); existed {
			return host, true
		}
	}

	if pool6 := h.fakeIPPool6; pool6 != nil {
		if host, existed := pool6.LookBack(ip); existed {
			return host, true
		}
	}

	if mapping := h.mapping; mapping != nil {
		if host, existed := mapping.Get(ip); existed && h.mappingAllowed(host) {
			return host, true
		}
	}

	return "", false
}

func (h *ResolverEnhancer) InsertHostByIP(ip netip.Addr, host string) {
	if mapping := h.mappingForHost(host); mapping != nil {
		mapping.Set(ip, host)
	}
}

// mappingAllowed checks one name against the immutable domain index. Keeping
// the filter nil outside redir-host preserves fake-IP and normal-mode behavior.
func (h *ResolverEnhancer) mappingAllowed(host string) bool {
	return h.redirHostFilter == nil || !h.redirHostFilter.Has(strings.TrimSuffix(host, "."))
}

// mappingForHost authorizes a whole answer's mapping writes with one lookup,
// instead of repeating the same domain match for every returned address.
func (h *ResolverEnhancer) mappingForHost(host string) *lru.LruCache[netip.Addr, string] {
	if h.mapping != nil && h.mappingAllowed(host) {
		return h.mapping
	}
	return nil
}

func (h *ResolverEnhancer) FlushFakeIP() error {
	var errs []error
	if pool := h.fakeIPPool; pool != nil {
		if err := pool.FlushFakeIP(); err != nil {
			errs = append(errs, err)
		}
	}
	if pool6 := h.fakeIPPool6; pool6 != nil {
		if err := pool6.FlushFakeIP(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (h *ResolverEnhancer) PatchFrom(o *ResolverEnhancer) {
	if h.mapping != nil && o.mapping != nil {
		if h.redirHostFilter == nil {
			o.mapping.CloneTo(h.mapping)
		} else {
			o.mapping.CloneToFiltered(h.mapping, func(_ netip.Addr, host string) bool {
				return h.mappingAllowed(host)
			})
		}
	}

	if h.fakeIPPool != nil && o.fakeIPPool != nil {
		h.fakeIPPool.CloneFrom(o.fakeIPPool)
	}

	if h.fakeIPPool6 != nil && o.fakeIPPool6 != nil {
		h.fakeIPPool6.CloneFrom(o.fakeIPPool6)
	}
}

func (h *ResolverEnhancer) StoreFakePoolState() {
	if h.fakeIPPool != nil {
		h.fakeIPPool.StoreState()
	}

	if h.fakeIPPool6 != nil {
		h.fakeIPPool6.StoreState()
	}
}

type EnhancerConfig struct {
	IPv6            bool
	EnhancedMode    C.DNSMode
	FakeIPPool      *fakeip.Pool
	FakeIPPool6     *fakeip.Pool
	FakeIPSkipper   *fakeip.Skipper
	FakeIPTTL       int
	RedirHostFilter *trie.DomainSet
	UseHosts        bool
}

func NewEnhancer(cfg EnhancerConfig) *ResolverEnhancer {
	e := &ResolverEnhancer{
		ipv6:     cfg.IPv6,
		mode:     cfg.EnhancedMode,
		useHosts: cfg.UseHosts,
	}
	if cfg.EnhancedMode == C.DNSMapping {
		e.redirHostFilter = cfg.RedirHostFilter
	}

	if cfg.EnhancedMode != C.DNSNormal {
		e.fakeIPPool = cfg.FakeIPPool
		if cfg.IPv6 {
			e.fakeIPPool6 = cfg.FakeIPPool6
		}
		e.fakeIPSkipper = cfg.FakeIPSkipper
		e.fakeIPTTL = cfg.FakeIPTTL
		if e.fakeIPTTL < 1 {
			e.fakeIPTTL = 1
		}
		e.mapping = lru.New(lru.WithSize[netip.Addr, string](4096))
	}

	return e
}

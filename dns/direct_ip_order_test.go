package dns

import (
	"github.com/metacubex/mihomo/tunnel"
	"testing"
)

func TestDNSOptimizationIPOrderOnlyWhenActive(t *testing.T) {
	routingTestEnable(t)
	r := &Resolver{ruleRouting: true, speedChecker: &directSpeedChecker{}}
	if !r.PreferIPOrder() {
		t.Fatal("active optimizer did not preserve IP order")
	}
	r.bootstrap = true
	if r.PreferIPOrder() {
		t.Fatal("bootstrap inherited business ordering")
	}
	r.bootstrap = false
	r.speedChecker = nil
	if r.PreferIPOrder() {
		t.Fatal("disabled optimizer changed IP selection")
	}
	r.speedChecker = &directSpeedChecker{}
	tunnel.SetMode(tunnel.Global)
	if r.PreferIPOrder() {
		t.Fatal("inactive rule routing changed IP selection")
	}
	if (*Resolver)(nil).PreferIPOrder() {
		t.Fatal("nil resolver changed IP selection")
	}
}

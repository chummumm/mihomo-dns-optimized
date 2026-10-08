package route

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/metacubex/http"
	"github.com/metacubex/http/httptest"
	"github.com/metacubex/mihomo/tunnel"
)

func TestDNSRuleRoutingConfigAPI(t *testing.T) {
	previous := tunnel.DNSRuleRoutingEnabled()
	defer tunnel.SetDNSRuleRouting(previous)
	for _, enabled := range []bool{false, true} {
		tunnel.SetDNSRuleRouting(enabled)
		response := httptest.NewRecorder()
		getConfigs(response, httptest.NewRequest(http.MethodGet, "/configs", nil))
		var general map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &general); err != nil {
			t.Fatal(err)
		}
		if general["dns-rule-routing"] != enabled {
			t.Fatalf("GET did not report effective flag: %s", response.Body.String())
		}
		if _, exists := general["dns-proxy-port"]; exists {
			t.Fatal("GET still reports a removed dedicated port")
		}
	}
}

func TestDNSRuleRoutingPatchRequiresFullReloadBeforeAnyMutation(t *testing.T) {
	previousFlag, previousMode := tunnel.DNSRuleRoutingEnabled(), tunnel.Mode()
	defer tunnel.SetDNSRuleRouting(previousFlag)
	defer tunnel.SetMode(previousMode)
	tunnel.SetDNSRuleRouting(false)
	tunnel.SetMode(tunnel.Rule)
	for _, body := range []string{
		`{"dns-rule-routing":true,"mode":"global"}`,
		`{"dns-rule-routing":false,"mode":"direct"}`,
		`{"dns-proxy-port":7853,"mode":"global"}`,
	} {
		response := httptest.NewRecorder()
		patchConfigs(response, httptest.NewRequest(http.MethodPatch, "/configs", strings.NewReader(body)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("PATCH should reject incomplete switch update: %d %s", response.Code, response.Body.String())
		}
		if tunnel.DNSRuleRoutingEnabled() || tunnel.Mode() != tunnel.Rule {
			t.Fatal("rejected PATCH partially changed the running configuration")
		}
	}
}

package route

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/metacubex/http"
	"github.com/metacubex/http/httptest"
	LC "github.com/metacubex/mihomo/listener/config"
	"github.com/metacubex/mihomo/tunnel"
)

func TestTunScalarPatchPreservesOmittedEnable(t *testing.T) {
	for _, test := range []struct {
		body    string
		enabled bool
	}{
		{`{"tun":{"mtu":1400}}`, true},
		{`{"tun":{"enable":false,"mtu":1400}}`, false},
	} {
		var patch configSchema
		if err := json.Unmarshal([]byte(test.body), &patch); err != nil {
			t.Fatal(err)
		}
		got := pointerOrDefaultTun(patch.Tun, LC.Tun{Enable: true, Device: "utun", MTU: 1500})
		if got.Enable != test.enabled || got.MTU != 1400 || got.Device != "utun" {
			t.Fatalf("scalar patch changed omitted TUN intent: %+v", got)
		}
	}
}

func TestManagedPolicyPatchRejectsBeforeOtherScalarChanges(t *testing.T) {
	oldMode := tunnel.Mode()
	t.Cleanup(func() { tunnel.SetMode(oldMode) })
	tunnel.SetMode(tunnel.Rule)
	for _, body := range []string{
		`{"transparent-proxy":{"enable":true},"mode":"direct"}`,
		`{"tproxy-port":-1,"mode":"direct"}`,
		`{"tproxy-port":65536,"mode":"direct"}`,
	} {
		response := httptest.NewRecorder()
		patchConfigs(response, httptest.NewRequest(http.MethodPatch, "/configs", strings.NewReader(body)))
		if response.Code != http.StatusBadRequest || tunnel.Mode() != tunnel.Rule {
			t.Fatalf("invalid policy patch partially applied scalar settings: %d %s", response.Code, response.Body.String())
		}
	}
}

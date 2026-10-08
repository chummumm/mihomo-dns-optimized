package route

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/metacubex/http"
	"github.com/metacubex/http/httptest"
	"github.com/metacubex/mihomo/listener"
)

func TestDNSProxyConfigAPIPatchAndGet(t *testing.T) {
	oldAllow, oldBind := listener.AllowLan(), listener.BindAddress()
	listener.SetAllowLan(false)
	listener.SetBindAddress("*")
	t.Cleanup(func() {
		listener.ReCreateDNSProxy(0, nil)
		listener.SetAllowLan(oldAllow)
		listener.SetBindAddress(oldBind)
	})
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	udp, err := net.ListenPacket("udp", probe.Addr().String())
	_ = probe.Close()
	if err != nil {
		t.Fatal(err)
	}
	_ = udp.Close()

	for _, test := range []struct {
		body string
		want int
	}{
		{fmt.Sprintf(`{"dns-proxy-port":%d}`, port), port},
		{`{}`, port}, // An omitted PATCH field retains the current port.
		{`{"dns-proxy-port":0}`, 0},
	} {
		request := httptest.NewRequest(http.MethodPatch, "/configs", strings.NewReader(test.body))
		response := httptest.NewRecorder()
		patchConfigs(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("PATCH %s: status %d body %s", test.body, response.Code, response.Body.String())
		}
		if got := listener.GetPorts().DNSProxyPort; got != test.want {
			t.Fatalf("PATCH %s: listener port=%d, want %d", test.body, got, test.want)
		}
		response = httptest.NewRecorder()
		getConfigs(response, httptest.NewRequest(http.MethodGet, "/configs", nil))
		var general map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &general); err != nil {
			t.Fatal(err)
		}
		if general["dns-proxy-port"] != float64(test.want) {
			t.Fatalf("GET after PATCH %s: dns-proxy-port=%v, want %d", test.body, general["dns-proxy-port"], test.want)
		}
	}
}

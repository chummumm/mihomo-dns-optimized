package route

import (
	"strings"
	"testing"

	"github.com/metacubex/http"
	"github.com/metacubex/http/httptest"
)

func TestDNSObservabilityRoutesAndValidation(t *testing.T) {
	for _, path := range []string{"/observability", "/observability/", "/observability/stats", "/observability/queries", "/observability/upstreams"} {
		response := httptest.NewRecorder()
		dnsRouter().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d, %s", path, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), ":null") {
			t.Fatalf("nullable API collection at %s: %s", path, response.Body.String())
		}
	}
	for _, query := range []string{"limit=0", "limit=201", "limit=bad", "cursor=-1", "cursor=0", "cursor=bad", "outcome=made-up"} {
		response := httptest.NewRecorder()
		dnsRouter().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/observability/queries?"+query, nil))
		if response.Code != http.StatusBadRequest || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("invalid filter accepted: %s: %d %s", query, response.Code, response.Body.String())
		}
	}
}

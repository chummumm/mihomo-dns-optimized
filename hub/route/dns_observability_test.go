package route

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/metacubex/mihomo/component/dnsstats"

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
	for _, query := range []string{"limit=0", "limit=201", "limit=bad", "cursor=-1", "cursor=0", "cursor=bad", "outcome=made-up", "anchor=-1", "anchor=0", "anchor=bad", "anchor=18446744073709551616"} {
		response := httptest.NewRecorder()
		dnsRouter().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/observability/queries?"+query, nil))
		if response.Code != http.StatusBadRequest || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("invalid filter accepted: %s: %d %s", query, response.Code, response.Body.String())
		}
	}
}

func TestDNSObservabilityPageIndexHTTP(t *testing.T) {
	for _, query := range []string{"", "anchor=1", "anchor=18446744073709551615&cursor=1&limit=1"} {
		response := httptest.NewRecorder()
		dnsRouter().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/observability/queries?"+query, nil))
		var page dnsstats.Page
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
			t.Fatalf("valid page index request failed: %s: %d %s", query, response.Code, response.Body.String())
		}
		if page.Total > page.Retained || len(page.PageCursors) == 0 || page.PageCursors[0] != "" || page.InstanceID == "" {
			t.Fatalf("page index metadata missing or inconsistent: %+v", page)
		}
		for _, field := range []string{"\"total\":", "\"page_cursors\":", "\"anchor_id\":"} {
			if !strings.Contains(response.Body.String(), field) {
				t.Fatalf("missing field %s in %s", field, response.Body.String())
			}
		}
	}
}

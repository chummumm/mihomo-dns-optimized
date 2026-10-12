package adapter

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// proxyDNSStatsIdentity is computed once, after provider overrides and before
// constructing the adapter. Runtime tunnel/dialer objects must not enter a stable
// configuration fingerprint. An opaque API dialer instead keeps instance scope.
func proxyDNSStatsIdentity(mapping map[string]any, opt proxyOption) string {
	if opt.DialerForAPI != nil {
		return ""
	}
	configuration := struct {
		Provider string         `json:"provider"`
		Proxy    map[string]any `json:"proxy"`
	}{Provider: opt.ProviderName, Proxy: mapping}
	// encoding/json sorts map keys, including nested maps. Keep all configured
	// options, including credentials and mux options, but retain only their hash.
	encoded, err := json.Marshal(configuration)
	if err != nil {
		// Custom API values need not be JSON-compatible. Do not reject an otherwise
		// valid adapter or merge it using a partial configuration fingerprint.
		return ""
	}
	return fmt.Sprintf("config-v1:%x", sha256.Sum256(encoded))
}

package config

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// These SmartDNS names deliberately retain DNS record-type capitalization.
// Reject casing mistakes explicitly: YAML's normal unknown-field behavior
// would otherwise silently disable the user's requested answer policy.
func (raw *RawDNS) UnmarshalYAML(node *yaml.Node) error {
	if err := validateDNSOptionNames(node, make(map[*yaml.Node]bool)); err != nil {
		return err
	}
	type plainRawDNS RawDNS
	decoded := plainRawDNS(*raw) // Preserve the defaults supplied by the caller.
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	// YAML skips a field's UnmarshalYAML method for null. Resolve the effective
	// mapping (including merges) so only an explicitly empty field disables
	// probes; an absent field must retain the automatic default.
	if decoded.SpeedCheckMode == nil {
		var fields map[string]yaml.Node
		if err := node.Decode(&fields); err != nil {
			return err
		}
		if _, present := fields["speed-check-mode"]; present {
			decoded.SpeedCheckMode = RawDNSSpeedCheckModes{}
		}
	}
	*raw = RawDNS(decoded)
	return nil
}

// JSON is also accepted by the configuration API. Use the same typed decoder
// so encoding/json's case-insensitive field matching cannot reintroduce aliases.
func (raw *RawDNS) UnmarshalJSON(data []byte) error {
	return yaml.Unmarshal(data, raw)
}

func validateDNSOptionNames(node *yaml.Node, visited map[*yaml.Node]bool) error {
	if node == nil || visited[node] {
		return nil // The YAML decoder itself reports cyclic/invalid aliases.
	}
	visited[node] = true
	switch node.Kind {
	case yaml.AliasNode:
		return validateDNSOptionNames(node.Alias, visited)
	case yaml.SequenceNode:
		// A YAML merge may import an ordered list of mappings.
		for _, item := range node.Content {
			if err := validateDNSOptionNames(item, visited); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			var name string
			if err := node.Content[i].Decode(&name); err != nil {
				return err
			}
			if strings.EqualFold(name, "prefetch-mode") || strings.EqualFold(name, "serve-expired-prefetch-time") {
				return fmt.Errorf("dns.%s is no longer supported; remove it and use dns.prefetch-domain to enable the fixed DIRECT prefetch policy", name)
			}
			for _, canonical := range []string{"force-no-CNAME", "force-AAAA-SOA", "force-qtype-SOA", "dualstack-ip-allow-force-AAAA"} {
				if name != canonical && strings.EqualFold(name, canonical) {
					return fmt.Errorf("dns.%s is not supported; use the case-sensitive option dns.%s", name, canonical)
				}
			}
			if name == "<<" {
				if err := validateDNSOptionNames(node.Content[i+1], visited); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

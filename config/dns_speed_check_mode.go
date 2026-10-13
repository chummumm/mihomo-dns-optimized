package config

import (
	"encoding/json"
	"fmt"

	"go.yaml.in/yaml/v3"
)

// A nil value selects the automatic default; a non-nil empty value disables
// probes. Keep this distinction through configuration parsing and serialization.
type RawDNSSpeedCheckModes []string

func (m *RawDNSSpeedCheckModes) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.ShortTag() != "!!str" {
			return fmt.Errorf("dns.speed-check-mode requires a mode string or a list of mode strings; use an empty value or [] to disable")
		}
		if node.Value == "" {
			*m = RawDNSSpeedCheckModes{}
		} else {
			*m = RawDNSSpeedCheckModes{node.Value}
		}
		return nil
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if item.ShortTag() != "!!str" {
				return fmt.Errorf("dns.speed-check-mode requires a list of mode strings")
			}
		}
		var modes []string
		if err := node.Decode(&modes); err != nil {
			return err
		}
		*m = modes
		return nil
	default:
		return fmt.Errorf("dns.speed-check-mode requires a mode string or a list of mode strings; use an empty value or [] to disable")
	}
}

func (m RawDNSSpeedCheckModes) MarshalYAML() (any, error) {
	if m == nil {
		return []string{"auto"}, nil
	}
	return []string(m), nil
}

func (m RawDNSSpeedCheckModes) MarshalJSON() ([]byte, error) {
	if m == nil {
		return json.Marshal([]string{"auto"})
	}
	return json.Marshal([]string(m))
}

package config

import (
	"errors"

	"go.yaml.in/yaml/v3"
)

// CoreUpdateInterval uses whole hours, with the same strict scalar decoding as
// the dashboard schedule. Fractional hours must never be silently truncated.
type CoreUpdateInterval int

func (h *CoreUpdateInterval) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return errors.New("core-update-interval must be an integer number of hours")
	}
	var hours int
	if err := node.Decode(&hours); err != nil {
		return err
	}
	*h = CoreUpdateInterval(hours)
	return nil
}

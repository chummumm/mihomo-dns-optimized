package config

import (
	"errors"

	"go.yaml.in/yaml/v3"
)

// GeoUpdateInterval is a whole number of hours, like the UI/core schedules.
// Reject fractional YAML values instead of silently truncating the interval.
type GeoUpdateInterval int

func (h *GeoUpdateInterval) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return errors.New("geo-update-interval must be an integer number of hours")
	}
	var hours int
	if err := node.Decode(&hours); err != nil {
		return err
	}
	*h = GeoUpdateInterval(hours)
	return nil
}

package config

import (
	"errors"

	"go.yaml.in/yaml/v3"
)

// UIUpdateInterval is a whole number of hours. YAML's default integer decoder
// accepts floats by truncation; reject them rather than silently changing the
// user's update schedule.
type UIUpdateInterval int

func (h *UIUpdateInterval) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return errors.New("external-ui-update-interval must be an integer number of hours")
	}
	var hours int
	if err := node.Decode(&hours); err != nil {
		return err
	}
	*h = UIUpdateInterval(hours)
	return nil
}

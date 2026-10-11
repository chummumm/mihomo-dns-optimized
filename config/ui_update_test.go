package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUIUpdaterConfig(t *testing.T) {
	for _, test := range []struct {
		name, yaml string
		enabled    bool
		hours      int
	}{
		{"default", "mixed-port: 7890\n", false, 24},
		{"enabled", "external-ui-auto-update: true\nexternal-ui-update-interval: 6\n", true, 6},
		{"disabled", "external-ui-auto-update: false\nexternal-ui-update-interval: 12\n", false, 12},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte(test.yaml))
			if err != nil {
				t.Fatal(err)
			}
			general, err := parseGeneral(raw)
			if err != nil {
				t.Fatal(err)
			}
			if general.ExternalUIAutoUpdate != test.enabled || general.ExternalUIUpdateInterval != test.hours {
				t.Fatalf("incorrect UI update policy: enabled=%v hours=%d", general.ExternalUIAutoUpdate, general.ExternalUIUpdateInterval)
			}
			data, err := json.Marshal(general)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["external-ui-auto-update"] != test.enabled || fields["external-ui-update-interval"] != float64(test.hours) {
				t.Fatalf("API omitted update policy: %s", data)
			}
		})
	}
}

func TestUIUpdaterConfigRejectsInvalidIntervals(t *testing.T) {
	for _, value := range []string{"0", "-1", "2562048", "9223372036854775808", "1.5", "true", "\"6\"", "[6]"} {
		t.Run(value, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("external-ui-update-interval: " + value + "\n"))
			if err == nil {
				_, err = parseGeneral(raw)
			}
			if err == nil {
				t.Fatal("invalid update interval was accepted")
			}
			// Decode errors may be emitted by YAML itself, but range validation
			// must name the setting so a bad schedule is straightforward to fix.
			if (value == "0" || value == "-1" || value == "2562048") && !strings.Contains(err.Error(), "external-ui-update-interval") {
				t.Fatalf("unhelpful interval error: %v", err)
			}
		})
	}
}

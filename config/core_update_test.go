package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCoreUpdaterConfig(t *testing.T) {
	for _, test := range []struct {
		name, yaml string
		enabled    bool
		hours      int
	}{
		{"default", "mixed-port: 7890\n", false, 24},
		{"enabled", "core-auto-update: true\ncore-update-interval: 6\n", true, 6},
		{"disabled", "core-auto-update: false\ncore-update-interval: 168\n", false, 168},
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
			if general.CoreAutoUpdate != test.enabled || general.CoreUpdateInterval != test.hours {
				t.Fatalf("incorrect core policy: %+v", general)
			}
			body, err := json.Marshal(general)
			if err != nil {
				t.Fatal(err)
			}
			var output map[string]any
			if err := json.Unmarshal(body, &output); err != nil {
				t.Fatal(err)
			}
			if output["core-auto-update"] != test.enabled || output["core-update-interval"] != float64(test.hours) {
				t.Fatalf("API omitted core update policy: %s", body)
			}
		})
	}
}

func TestCoreUpdaterConfigRejectsInvalidIntervals(t *testing.T) {
	for _, value := range []string{"0", "-1", "2562048", "9223372036854775808", "1.5", "true", "\"6\"", "[6]"} {
		t.Run(value, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("core-update-interval: " + value + "\n"))
			if err == nil {
				_, err = parseGeneral(raw)
			}
			if err == nil {
				t.Fatal("invalid core interval was accepted")
			}
			if (value == "0" || value == "-1" || value == "2562048") && !strings.Contains(err.Error(), "core-update-interval") {
				t.Fatalf("interval error did not identify the setting: %v", err)
			}
		})
	}
}

func TestUIUpdaterConfigIgnoresLegacySourceAndPath(t *testing.T) {
	raw, err := UnmarshalRawConfig([]byte("external-ui: /outside/the/configuration/home\nexternal-ui-url: https://old.example/ui.zip\nexternal-ui-name: ../../other\n"))
	if err != nil {
		t.Fatal(err)
	}
	controller, err := parseController(raw)
	if err != nil {
		t.Fatalf("obsolete source options prevented loading an existing config: %v", err)
	}
	if controller.ExternalUI != "" || controller.ExternalUIURL != "" || controller.ExternalUIName != "" {
		t.Fatal("obsolete source options reached the active controller")
	}
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := json.Unmarshal(body, &output); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"external-ui", "external-ui-url", "external-ui-name"} {
		if _, present := output[key]; present {
			t.Fatalf("obsolete option %s was exposed in config output", key)
		}
	}
}

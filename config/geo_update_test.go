package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGeoAutomaticConfig(t *testing.T) {
	for _, test := range []struct {
		name, yaml string
		enabled    bool
		hours      int
	}{
		{"default", "mixed-port: 7890\n", false, 24},
		{"enabled", "geo-auto-update: true\ngeo-update-interval: 6\n", true, 6},
		{"disabled", "geo-auto-update: false\ngeo-update-interval: 12\n", false, 12},
		{"maximum", "geo-update-interval: 2562047\n", false, 2562047},
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
			if general.GeoAutoUpdate != test.enabled || general.GeoUpdateInterval != test.hours {
				t.Fatalf("GeoData policy = enabled %v, interval %d", general.GeoAutoUpdate, general.GeoUpdateInterval)
			}
			data, err := json.Marshal(general)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["geo-auto-update"] != test.enabled || fields["geo-update-interval"] != float64(test.hours) {
				t.Fatalf("API did not preserve the numeric GeoData schedule: %s", data)
			}
		})
	}
}

func TestGeoAutomaticConfigRejectsInvalidIntervals(t *testing.T) {
	for _, value := range []string{"0", "-1", "2562048", "9223372036854775808", "1.5", "true", "\"6\"", "[6]"} {
		t.Run(value, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("geo-update-interval: " + value + "\n"))
			if err == nil {
				_, err = parseGeneral(raw)
			}
			if err == nil {
				t.Fatal("invalid GeoData update interval was accepted")
			}
			if (value == "0" || value == "-1" || value == "2562048") && !strings.Contains(err.Error(), "geo-update-interval") {
				t.Fatalf("interval error did not identify the field: %v", err)
			}
		})
	}
}

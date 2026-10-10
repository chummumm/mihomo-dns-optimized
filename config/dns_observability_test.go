package config

import "testing"

func TestDNSObservabilityConfiguration(t *testing.T) {
	for _, test := range []struct {
		input string
		want  bool
	}{{"", true}, {"dns:\n  observability: true\n", true}, {"dns:\n  observability: false\n", false}} {
		raw, err := UnmarshalRawConfig([]byte(test.input))
		if err != nil {
			t.Fatal(err)
		}
		config, err := parseDNS(raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if config.Observability != test.want {
			t.Fatalf("configuration %q got %v, want %v", test.input, config.Observability, test.want)
		}
	}
}

package config

import (
	"encoding/json"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/metacubex/mihomo/common/yaml"
)

func TestDNSSpeedCheckAutoSelectionPreservesExplicitModes(t *testing.T) {
	for _, test := range []struct {
		name, field string
		want        []string
	}{
		{name: "omitted-selects-auto", want: nil},
		{name: "scalar-auto", field: "  speed-check-mode: auto\n", want: []string{"auto"}},
		{name: "list-auto", field: "  speed-check-mode: [auto]\n", want: []string{"auto"}},
		{name: "empty-value-disables", field: "  speed-check-mode:\n", want: []string{}},
		{name: "null-disables", field: "  speed-check-mode: null\n", want: []string{}},
		{name: "tilde-disables", field: "  speed-check-mode: ~\n", want: []string{}},
		{name: "empty-string-disables", field: "  speed-check-mode: \"\"\n", want: []string{}},
		{name: "empty-list-disables", field: "  speed-check-mode: []\n", want: []string{}},
		{name: "bare-tcp-keeps-connect", field: "  speed-check-mode: [tcp]\n", want: []string{"tcp"}},
		{name: "explicit-tcp-and-ping", field: "  speed-check-mode:\n    - tcp:443\n    - ping\n", want: []string{"tcp:443", "ping"}},
		// This exceeds the stage limit before equivalent explicit modes are
		// deduplicated. Config validation must still accept three actual stages.
		{name: "equivalent-stages-deduplicate", field: "  speed-check-mode: [tcp, tcp:80, tcp:443, ping, tcp:80]\n", want: []string{"tcp", "tcp:80", "tcp:443", "ping", "tcp:80"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("dns:\n  enable: true\n  nameserver: [192.0.2.53]\n" + test.field))
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseDNS(raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.SpeedCheck.Mode, test.want) {
				t.Fatalf("speed-check-mode=%#v, want %#v; absent and explicitly empty settings must remain distinct", got.SpeedCheck.Mode, test.want)
			}
			if got.CacheOptions.Prefetch || got.DualStack.Enabled {
				t.Fatal("selecting a speed-check mode enabled an independent optimization")
			}
		})
	}
}

func TestDNSSpeedCheckAutoRejectsAmbiguousAndInvalidModes(t *testing.T) {
	for _, modes := range []string{
		"[auto, tcp:443]", "[ping, auto]", "[auto, none]", "[none, tcp:80]",
		"[auto:443]", "[tcp-auto:443]", "[\"tcp:\"]", "[tcp:0]", "[tcp:-1]", "[tcp:65536]", "[tcp:invalid]",
		"[tcp:80, tcp:81, tcp:82, tcp:83, ping]",
	} {
		t.Run(modes, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("dns:\n  speed-check-mode: " + modes + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseDNS(raw, nil); err == nil || !strings.Contains(err.Error(), "speed-check-mode") {
				t.Fatalf("invalid or ambiguous speed-check-mode %s accepted or unclear error: %v", modes, err)
			}
		})
	}
}

func TestDNSSpeedCheckLegacyNoneRequiresEmptySetting(t *testing.T) {
	for _, value := range []string{"none", "NONE", "[none]", "[None]", "[NONE]", "[none, ping]", "[ping, NoNe]"} {
		t.Run(value, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte("dns:\n  speed-check-mode: " + value + "\n"))
			if err == nil {
				_, err = parseDNS(raw, nil)
			}
			if err == nil || !strings.Contains(err.Error(), "speed-check-mode") || !strings.Contains(err.Error(), "[]") || !strings.Contains(strings.ToLower(err.Error()), "none") {
				t.Fatalf("legacy none setting %s did not explain how to disable with an empty setting: %v", value, err)
			}
		})
	}
}

func TestDNSSpeedCheckExplicitSYNRetainsPlatformRequirement(t *testing.T) {
	for _, modes := range []string{"[tcp-syn]", "[tcp-syn:443, tcp:80, ping]"} {
		raw, err := UnmarshalRawConfig([]byte("dns:\n  speed-check-mode: " + modes + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = parseDNS(raw, nil)
		if runtime.GOOS == "linux" {
			if err != nil {
				t.Fatalf("explicit Linux SYN mode rejected: %v", err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "requires Linux") {
			t.Fatalf("explicit SYN on %s did not retain its platform requirement: %v", runtime.GOOS, err)
		}
	}
}

func TestDNSSpeedCheckYAMLAliasesAndMergesPreserveEmptyOverrides(t *testing.T) {
	for _, test := range []struct {
		name, document string
		want           []string
	}{
		{"alias-auto", "probe-choice: &choice auto\ndns:\n  speed-check-mode: *choice\n", []string{"auto"}},
		{"alias-null", "probe-choice: &choice null\ndns:\n  speed-check-mode: *choice\n", []string{}},
		{"alias-dns-map", "defaults: &defaults {speed-check-mode: null}\ndns: *defaults\n", []string{}},
		{"merge-omitted", "defaults: &defaults {enable: true}\ndns:\n  <<: *defaults\n", nil},
		{"merge-null", "defaults: &defaults {speed-check-mode: null}\ndns:\n  <<: *defaults\n", []string{}},
		{"explicit-auto-overrides-merged-null", "defaults: &defaults {speed-check-mode: null}\ndns:\n  <<: *defaults\n  speed-check-mode: auto\n", []string{"auto"}},
		{"explicit-null-overrides-merged-auto", "defaults: &defaults {speed-check-mode: auto}\ndns:\n  <<: *defaults\n  speed-check-mode: null\n", []string{}},
		{"explicit-empty-before-merge", "defaults: &defaults {speed-check-mode: auto}\ndns:\n  speed-check-mode: []\n  <<: *defaults\n", []string{}},
		{"first-merged-null-wins", "disabled: &disabled {speed-check-mode: null}\nautomatic: &automatic {speed-check-mode: auto}\ndns:\n  <<: [*disabled, *automatic]\n", []string{}},
		{"first-merged-auto-wins", "disabled: &disabled {speed-check-mode: null}\nautomatic: &automatic {speed-check-mode: auto}\ndns:\n  <<: [*automatic, *disabled]\n", []string{"auto"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := UnmarshalRawConfig([]byte(test.document))
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseDNS(raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.SpeedCheck.Mode, test.want) {
				t.Fatalf("alias/merge changed selected speed-check-mode: got %#v want %#v", got.SpeedCheck.Mode, test.want)
			}
		})
	}
}

func TestDNSSpeedCheckJSONPreservesAbsentAndEmptySettings(t *testing.T) {
	for _, test := range []struct {
		name, document string
		want           []string
	}{
		{"omitted", `{}`, nil},
		{"null", `{"speed-check-mode":null}`, []string{}},
		{"empty-list", `{"speed-check-mode":[]}`, []string{}},
		{"empty-string", `{"speed-check-mode":""}`, []string{}},
		{"scalar-auto", `{"speed-check-mode":"auto"}`, []string{"auto"}},
		{"list-auto", `{"speed-check-mode":["auto"]}`, []string{"auto"}},
		{"explicit-modes", `{"speed-check-mode":["tcp:443","ping"]}`, []string{"tcp:443", "ping"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var raw RawDNS
			if err := json.Unmarshal([]byte(test.document), &raw); err != nil {
				t.Fatal(err)
			}
			got, _, err := parseDNSOptimizations(raw)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Mode, test.want) {
				t.Fatalf("JSON setting changed automatic/disabled selection: got %#v want %#v", got.Mode, test.want)
			}
		})
	}
}

func TestDNSSpeedCheckSerializationRetainsSelection(t *testing.T) {
	for _, test := range []struct {
		name, document string
		want           []string
	}{
		{"default-auto", `{}`, []string{"auto"}},
		{"explicit-auto", `{"speed-check-mode":"auto"}`, []string{"auto"}},
		{"disabled-null", `{"speed-check-mode":null}`, []string{}},
		{"disabled-list", `{"speed-check-mode":[]}`, []string{}},
		{"disabled-string", `{"speed-check-mode":""}`, []string{}},
		{"explicit-tcp", `{"speed-check-mode":["tcp:443","ping"]}`, []string{"tcp:443", "ping"}},
	} {
		for _, codec := range []struct {
			name      string
			marshal   func(any) ([]byte, error)
			unmarshal func([]byte, any) error
		}{
			{"YAML", yaml.Marshal, yaml.Unmarshal},
			{"JSON", json.Marshal, json.Unmarshal},
		} {
			t.Run(test.name+"/"+codec.name, func(t *testing.T) {
				var raw RawDNS
				if err := json.Unmarshal([]byte(test.document), &raw); err != nil {
					t.Fatal(err)
				}
				for round := 0; round < 2; round++ {
					wire, err := codec.marshal(raw)
					if err != nil {
						t.Fatal(err)
					}
					var decoded RawDNS
					if err := codec.unmarshal(wire, &decoded); err != nil {
						t.Fatalf("cannot read serialized DNS configuration: %v", err)
					}
					got, _, err := parseDNSOptimizations(decoded)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got.Mode, test.want) {
						t.Fatalf("round %d changed selected speed-check-mode: got %#v want %#v", round+1, got.Mode, test.want)
					}
					raw = decoded
				}
			})
		}
	}
}

func TestDNSSpeedCheckDecodeIntoExistingReceiver(t *testing.T) {
	for _, test := range []struct {
		name, document string
		want           []string
	}{
		{"omitted-preserves-existing", `{}`, []string{"tcp:443"}},
		{"null-disables", `{"speed-check-mode":null}`, []string{}},
		{"empty-list-disables", `{"speed-check-mode":[]}`, []string{}},
		{"empty-string-disables", `{"speed-check-mode":""}`, []string{}},
		{"auto-replaces-explicit", `{"speed-check-mode":"auto"}`, []string{"auto"}},
	} {
		for _, codec := range []struct {
			name      string
			unmarshal func([]byte, any) error
		}{
			{"YAML", yaml.Unmarshal},
			{"JSON", json.Unmarshal},
		} {
			t.Run(test.name+"/"+codec.name, func(t *testing.T) {
				raw := RawDNS{SpeedCheckMode: RawDNSSpeedCheckModes{"tcp:443"}}
				if err := codec.unmarshal([]byte(test.document), &raw); err != nil {
					t.Fatal(err)
				}
				got, _, err := parseDNSOptimizations(raw)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got.Mode, test.want) {
					t.Fatalf("decoding over an existing configuration changed empty/absent semantics: got %#v want %#v", got.Mode, test.want)
				}
			})
		}
	}
}

func TestDNSSpeedCheckRejectsNonStringModes(t *testing.T) {
	for _, value := range []string{"true", "12", "[12]", "[true]", "[null]", "[[]]", "{mode: auto}"} {
		t.Run("YAML/"+value, func(t *testing.T) {
			_, err := UnmarshalRawConfig([]byte("dns:\n  speed-check-mode: " + value + "\n"))
			if err == nil || !strings.Contains(err.Error(), "speed-check-mode") {
				t.Fatalf("non-string speed-check-mode was accepted or lacked option context: %v", err)
			}
		})
	}
	for _, value := range []string{"true", "12", "[12]", "[true]", "[null]", "[[]]", `{"mode":"auto"}`} {
		t.Run("JSON/"+value, func(t *testing.T) {
			var raw RawDNS
			err := json.Unmarshal([]byte(`{"speed-check-mode":`+value+`}`), &raw)
			if err == nil || !strings.Contains(err.Error(), "speed-check-mode") {
				t.Fatalf("non-string speed-check-mode was accepted or lacked option context: %v", err)
			}
		})
	}
}

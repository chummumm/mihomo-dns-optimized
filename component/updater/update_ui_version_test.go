package updater

import (
	"strings"
	"testing"
)

func TestUIUpdaterVersionOrdering(t *testing.T) {
	tests := []struct {
		name, latest, current string
		want                  int
	}{
		{"zero", "0.0.0", "0.0.0", 0},
		{"patch", "0.3.10", "0.3.9", 1},
		{"minor", "0.10.0", "0.9.100", 1},
		{"major", "10.0.0", "9.999.999", 1},
		{"large major", "922337203685477580800.0.0", "922337203685477580799.0.0", 1},
		{"large patch", "1.0.100000000000000000000", "1.0.99999999999999999999", 1},
		{"numeric prerelease", "1.2.3-10", "1.2.3-9", 1},
		{"large numeric prerelease", "1.2.3-922337203685477580800", "1.2.3-922337203685477580799", 1},
		{"nonnumeric outranks numeric", "1.2.3-alpha", "1.2.3-99999999999999999999", 1},
		{"additional prerelease identifier", "1.2.3-alpha.0", "1.2.3-alpha", 1},
		{"ASCII case", "1.2.3-alpha", "1.2.3-ZETA", 1},
		{"nonnumeric lexical order", "1.2.3-alpha10", "1.2.3-alpha2", -1},
		{"core outranks prerelease", "1.2.4-alpha", "1.2.3", 1},
		{"release outranks prerelease", "1.2.3", "1.2.3-rc.99", 1},
		{"build ignored", "1.2.3+2025", "1.2.3+2024", 0},
		{"build optional", "1.2.3", "1.2.3+0001", 0},
		{"prerelease build ignored", "1.2.3-alpha.1+a.b", "1.2.3-alpha.1+0002", 0},
		{"latest prefix", "v1.2.3", "1.2.3", 0},
		{"current prefix", "1.2.3", "v1.2.3", 0},
		{"prefixed upgrade", "v1.2.4-rc.1+build", "v1.2.3+build", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, valid := compareUIVersions(tt.latest, tt.current)
			if !valid || got != tt.want {
				t.Fatalf("compareUIVersions(%q, %q) = (%d, %t), want (%d, true)", tt.latest, tt.current, got, valid, tt.want)
			}
			got, valid = compareUIVersions(tt.current, tt.latest)
			if !valid || got != -tt.want {
				t.Fatalf("reverse comparison = (%d, %t), want (%d, true)", got, valid, -tt.want)
			}
		})
	}
}

func TestUIUpdaterVersionPrereleaseSequence(t *testing.T) {
	// The complete precedence example from https://semver.org/#spec-item-11.
	versions := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0",
	}
	for i, latest := range versions {
		for j, current := range versions {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got, valid := compareUIVersions(latest, current); !valid || got != want {
				t.Errorf("compareUIVersions(%q, %q) = (%d, %t), want (%d, true)", latest, current, got, valid, want)
			}
		}
	}
}

func TestUIUpdaterVersionValidSyntax(t *testing.T) {
	versions := []string{
		"1.0.0-0.3.7", "1.0.0-x.7.z.92", "1.0.0-x-y-z.--", "1.0.0--",
		"1.0.0-01a", "1.0.0-alpha+001", "1.0.0+20130313144700",
		"1.0.0-beta+exp.sha.5114f85", "1.0.0+21AF26D3----117B344092BD",
		"1.2.3+" + strings.Repeat("a", 58), // Exactly 64 bytes.
		"v1.2.3+" + strings.Repeat("a", 57),
		"1" + strings.Repeat("0", 59) + ".0.0", // No integer-size limit.
	}
	for _, version := range versions {
		if got, valid := compareUIVersions(version, version); !valid || got != 0 {
			t.Errorf("valid version %q compared with itself = (%d, %t)", version, got, valid)
		}
	}
}

func TestUIUpdaterVersionRejectsInvalid(t *testing.T) {
	versions := []string{
		"", "v", "vv1.2.3", "V1.2.3", "v 1.2.3",
		"1", "1.2", "1.2.3.4", ".1.2", "1..2", "1.2.",
		"01.2.3", "1.02.3", "1.2.03", "-1.2.3", "+1.2.3", "1.2.-3",
		"1.2.3-", "1.2.3-01", "1.2.3-alpha.01", "1.2.3-.alpha",
		"1.2.3-alpha.", "1.2.3-alpha..beta", "1.2.3-alpha_beta",
		"1.2.3+", "1.2.3+.build", "1.2.3+build.", "1.2.3+build..info",
		"1.2.3+build+other", "1.2.3+build_info", "1.2.3-+build",
		" 1.2.3", "1.2.3 ", "1.2.3\n", "1.2.3\r", "1.2.3\t", "1.2.3\x00",
		"１.2.3", "1.2.٣", "1.2.3-β", "1.2.3+构建", "1.2.3-alpha/beta",
		"1.2.3+" + strings.Repeat("a", 59),
		"v1.2.3+" + strings.Repeat("a", 58),
	}
	for _, version := range versions {
		for _, pair := range [][2]string{{version, "1.2.3"}, {"1.2.3", version}, {version, version}} {
			if got, valid := compareUIVersions(pair[0], pair[1]); valid || got != 0 {
				t.Errorf("compareUIVersions(%q, %q) = (%d, %t), want (0, false)", pair[0], pair[1], got, valid)
			}
		}
	}
}

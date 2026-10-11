package updater

import "strings"

type uiVersion struct {
	core       [3]string
	prerelease []string
}

// compareUIVersions compares SemVer 2.0 precedence, allowing one leading "v".
// It returns -1, 0 or 1 for latest relative to current. Invalid or unavailable
// versions return valid=false; callers must not infer an update from a digest.
func compareUIVersions(latest, current string) (comparison int, valid bool) {
	left, ok := parseUIVersion(latest)
	if !ok {
		return 0, false
	}
	right, ok := parseUIVersion(current)
	if !ok {
		return 0, false
	}
	for i := range left.core {
		if n := compareUIVersionNumbers(left.core[i], right.core[i]); n != 0 {
			return n, true
		}
	}
	if len(left.prerelease) == 0 && len(right.prerelease) > 0 {
		return 1, true
	}
	if len(right.prerelease) == 0 && len(left.prerelease) > 0 {
		return -1, true
	}
	for i := 0; i < len(left.prerelease) && i < len(right.prerelease); i++ {
		a, b := left.prerelease[i], right.prerelease[i]
		aNumber, bNumber := uiVersionIsNumeric(a), uiVersionIsNumeric(b)
		var n int
		switch {
		case aNumber && bNumber:
			n = compareUIVersionNumbers(a, b)
		case aNumber:
			n = -1
		case bNumber:
			n = 1
		default:
			n = strings.Compare(a, b)
		}
		if n != 0 {
			return n, true
		}
	}
	if len(left.prerelease) < len(right.prerelease) {
		return -1, true
	}
	if len(left.prerelease) > len(right.prerelease) {
		return 1, true
	}
	return 0, true
}

func parseUIVersion(version string) (result uiVersion, valid bool) {
	// Match the UI metadata's byte limit before removing the optional prefix.
	if len(version) == 0 || len(version) > 64 {
		return result, false
	}
	version = strings.TrimPrefix(version, "v")
	core, build, hasBuild := strings.Cut(version, "+")
	if hasBuild && !validUIVersionIdentifiers(build, false) {
		return result, false
	}
	core, prerelease, hasPrerelease := strings.Cut(core, "-")
	if hasPrerelease {
		if !validUIVersionIdentifiers(prerelease, true) {
			return result, false
		}
		result.prerelease = strings.Split(prerelease, ".")
	}
	parts := strings.Split(core, ".")
	if len(parts) != len(result.core) {
		return result, false
	}
	for i, part := range parts {
		if !uiVersionIsNumeric(part) || (len(part) > 1 && part[0] == '0') {
			return result, false
		}
		result.core[i] = part
	}
	return result, true
}

func validUIVersionIdentifiers(value string, prerelease bool) bool {
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		for i := 0; i < len(identifier); i++ {
			c := identifier[i]
			if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '-') {
				return false
			}
		}
		if prerelease && len(identifier) > 1 && identifier[0] == '0' && uiVersionIsNumeric(identifier) {
			return false
		}
	}
	return true
}

func uiVersionIsNumeric(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

// Validated numeric identifiers have no leading zeros. Comparing their lengths
// before their digits preserves numeric order without a machine-integer limit.
func compareUIVersionNumbers(left, right string) int {
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return strings.Compare(left, right)
}

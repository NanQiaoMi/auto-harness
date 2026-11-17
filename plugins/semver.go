package plugins

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Version is a stable semantic version. Registry artifacts use the
// MAJOR.MINOR.PATCH subset; prerelease and build identifiers are not
// accepted because they would make plugin activation ambiguous.
type Version struct {
	Major uint64
	Minor uint64
	Patch uint64
}

// ParseVersion parses a stable MAJOR.MINOR.PATCH version.
func ParseVersion(value string) (Version, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version %q must be MAJOR.MINOR.PATCH", value)
	}
	numbers := make([]uint64, 3)
	for i, part := range parts {
		if part == "" {
			return Version{}, fmt.Errorf("version %q has an empty component", value)
		}
		if len(part) > 1 && part[0] == '0' {
			return Version{}, fmt.Errorf("version %q has a leading zero", value)
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("version %q: %w", value, err)
		}
		numbers[i] = number
	}
	return Version{Major: numbers[0], Minor: numbers[1], Patch: numbers[2]}, nil
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

func (v Version) Compare(other Version) int {
	switch {
	case v.Major < other.Major:
		return -1
	case v.Major > other.Major:
		return 1
	case v.Minor < other.Minor:
		return -1
	case v.Minor > other.Minor:
		return 1
	case v.Patch < other.Patch:
		return -1
	case v.Patch > other.Patch:
		return 1
	default:
		return 0
	}
}

// Constraint is a parsed dependency constraint. Comparisons are
// combined with logical AND, matching the usual semver range syntax.
type Constraint struct {
	raw    string
	ranges []versionRange
}

type versionRange struct {
	op      string
	version Version
}

// ParseConstraint parses *, exact versions, comparison operators,
// caret compatibility and tilde patch ranges. Multiple comparators are
// separated by whitespace and all must match.
func ParseConstraint(value string) (Constraint, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "*"
	}
	if value == "*" {
		return Constraint{raw: value}, nil
	}
	tokens := strings.Fields(value)
	if len(tokens) == 0 {
		return Constraint{}, fmt.Errorf("empty version constraint")
	}
	constraint := Constraint{raw: value}
	for _, token := range tokens {
		ranges, err := parseConstraintToken(token)
		if err != nil {
			return Constraint{}, err
		}
		constraint.ranges = append(constraint.ranges, ranges...)
	}
	return constraint, nil
}

func parseConstraintToken(token string) ([]versionRange, error) {
	prefixes := []string{">=", "<=", ">", "<", "=", "^", "~"}
	operator := ""
	value := token
	for _, prefix := range prefixes {
		if strings.HasPrefix(token, prefix) {
			operator = prefix
			value = strings.TrimSpace(strings.TrimPrefix(token, prefix))
			break
		}
	}
	version, err := ParseVersion(value)
	if err != nil {
		return nil, fmt.Errorf("version constraint %q: %w", token, err)
	}
	switch operator {
	case "", "=":
		return []versionRange{{op: "=", version: version}}, nil
	case ">", ">=", "<", "<=":
		return []versionRange{{op: operator, version: version}}, nil
	case "~":
		if version.Minor == ^uint64(0) {
			return nil, fmt.Errorf("version constraint %q exceeds the supported range", token)
		}
		upper := Version{Major: version.Major, Minor: version.Minor + 1}
		return []versionRange{
			{op: ">=", version: version},
			{op: "<", version: upper},
		}, nil
	case "^":
		var upper Version
		switch {
		case version.Major > 0:
			if version.Major == ^uint64(0) {
				return nil, fmt.Errorf("version constraint %q exceeds the supported range", token)
			}
			upper = Version{Major: version.Major + 1}
		case version.Minor > 0:
			if version.Minor == ^uint64(0) {
				return nil, fmt.Errorf("version constraint %q exceeds the supported range", token)
			}
			upper = Version{Minor: version.Minor + 1}
		default:
			if version.Patch == ^uint64(0) {
				return nil, fmt.Errorf("version constraint %q exceeds the supported range", token)
			}
			upper = Version{Patch: version.Patch + 1}
		}
		return []versionRange{
			{op: ">=", version: version},
			{op: "<", version: upper},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported version constraint %q", token)
	}
}

func (c Constraint) String() string {
	return c.raw
}

func (c Constraint) Match(version Version) bool {
	for _, item := range c.ranges {
		comparison := version.Compare(item.version)
		switch item.op {
		case "=":
			if comparison != 0 {
				return false
			}
		case ">":
			if comparison <= 0 {
				return false
			}
		case ">=":
			if comparison < 0 {
				return false
			}
		case "<":
			if comparison >= 0 {
				return false
			}
		case "<=":
			if comparison > 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func sortVersionsDescending(versions []string) ([]string, error) {
	parsed := make([]struct {
		raw     string
		version Version
	}, 0, len(versions))
	for _, raw := range versions {
		version, err := ParseVersion(raw)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, struct {
			raw     string
			version Version
		}{raw: raw, version: version})
	}
	sort.Slice(parsed, func(i, j int) bool {
		return parsed[i].version.Compare(parsed[j].version) > 0
	})
	out := make([]string, 0, len(parsed))
	for _, item := range parsed {
		out = append(out, item.raw)
	}
	return out, nil
}

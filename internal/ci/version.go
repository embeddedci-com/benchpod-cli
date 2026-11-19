package ci

import (
	"fmt"
	"strconv"
	"strings"
)

// VersionRange is a `require.pod_firmware` range: comparisons joined by commas, all of which must
// hold (">=3.10.8, <4").
type VersionRange []versionCond

type versionCond struct {
	op string
	v  [3]int
}

// ParseVersionRange reads ">=3.10.12", ">3.1,<4", "==3.10.1", "3.10.1" (same as ==).
func ParseVersionRange(s string) (VersionRange, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out VersionRange
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("%q has an empty condition", s)
		}
		op := "=="
		for _, o := range []string{">=", "<=", "==", ">", "<", "="} {
			if strings.HasPrefix(part, o) {
				op = o
				part = strings.TrimSpace(part[len(o):])
				break
			}
		}
		if op == "=" {
			op = "=="
		}
		v, ok := ParseVersion(part)
		if !ok {
			return nil, fmt.Errorf("%q is not a version (use >=3.10.12, <4, ==3.10.1)", part)
		}
		out = append(out, versionCond{op: op, v: v})
	}
	return out, nil
}

// ParseVersion reads "3.10.12", "v3.10", "3" (missing parts are 0). A suffix after the numbers
// ("-dev", "+abc") is ignored.
func ParseVersion(s string) ([3]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	var v [3]int
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 || parts[0] == "" {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func compareVersions(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Allows reports whether version satisfies every condition. An unreadable version never does.
func (r VersionRange) Allows(version string) bool {
	if len(r) == 0 {
		return true
	}
	v, ok := ParseVersion(version)
	if !ok {
		return false
	}
	for _, c := range r {
		cmp := compareVersions(v, c.v)
		var hold bool
		switch c.op {
		case ">=":
			hold = cmp >= 0
		case ">":
			hold = cmp > 0
		case "<=":
			hold = cmp <= 0
		case "<":
			hold = cmp < 0
		default:
			hold = cmp == 0
		}
		if !hold {
			return false
		}
	}
	return true
}

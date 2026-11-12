// Package version orders CrowdVM release versions: v1.2.3, pre-releases
// such as 1.2.3-rc.1 and nightlies (1.2.3-nightly.20261112.42), and
// `git describe` builds (v1.2.3-4-gabc1234[-dirty]). "dev", bare commit
// hashes and other strings cannot be ordered.
package version

import (
	"regexp"
	"strconv"
	"strings"
)

// V is a parsed release version.
type V struct {
	core  [3]int
	pre   string // pre-release identifiers (rc.1, nightly.20261112.42); "" for a release
	after int    // commits after the tag (describe suffix)
}

var (
	versionRe  = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?(?:-(.+))?$`)
	describeRe = regexp.MustCompile(`(?:^|-)(\d+)-g[0-9a-f]+$`)
)

// Parse reads a release version; ok is false for strings that cannot be
// ordered.
func Parse(s string) (v V, ok bool) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return v, false
	}
	for i := 0; i < 3; i++ {
		v.core[i], _ = strconv.Atoi(m[i+1])
	}
	suf := strings.TrimSuffix(m[4], "-dirty")
	if d := describeRe.FindStringSubmatchIndex(suf); d != nil {
		v.after, _ = strconv.Atoi(suf[d[2]:d[3]])
		suf = suf[:d[0]]
	}
	v.pre = suf
	return v, true
}

// Compare returns -1, 0 or 1 for a < b, a == b, a > b; ok is false when
// either side is not a release version.
func Compare(a, b string) (int, bool) {
	va, oka := Parse(a)
	vb, okb := Parse(b)
	if !oka || !okb {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if va.core[i] != vb.core[i] {
			return sign(va.core[i] - vb.core[i]), true
		}
	}
	if c := comparePre(va.pre, vb.pre); c != 0 {
		return c, true
	}
	return sign(va.after - vb.after), true
}

// comparePre orders pre-releases as semver does: none > any, then
// dot-separated identifiers left to right, numbers numerically and below
// words, so nightly.20261113.5 > nightly.20261112.9 and rc.10 > rc.9.
func comparePre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		switch {
		case ea == nil && eb == nil:
			if na != nb {
				return sign(na - nb)
			}
		case ea == nil:
			return -1
		case eb == nil:
			return 1
		default:
			if c := strings.Compare(pa[i], pb[i]); c != 0 {
				return c
			}
		}
	}
	return sign(len(pa) - len(pb))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

package policy

import (
	"fmt"
	"regexp"
	"strings"
)

var packageID = regexp.MustCompile(`^[A-Za-z0-9_]+([.-][A-Za-z0-9_]+)*$`)

type Allowlist struct {
	exact    map[string]struct{}
	prefixes []string
}

func ValidID(id string) bool {
	return len(id) <= 100 && packageID.MatchString(id)
}

func New(patterns []string) (*Allowlist, error) {
	list := &Allowlist{exact: make(map[string]struct{})}
	for _, pattern := range patterns {
		normalized := strings.ToLower(pattern)
		if strings.HasSuffix(normalized, "*") {
			prefix := strings.TrimSuffix(normalized, "*")
			// A prefix may end at a package-name separator, as in Ddth.*.
			stem := prefix
			if strings.HasSuffix(stem, ".") || strings.HasSuffix(stem, "-") {
				stem = stem[:len(stem)-1]
			}
			if prefix == "" || len(prefix) > 100 || !ValidID(stem) || strings.Contains(prefix, "*") {
				return nil, fmt.Errorf("invalid package prefix %q", pattern)
			}
			list.prefixes = append(list.prefixes, prefix)
		} else {
			if !ValidID(normalized) {
				return nil, fmt.Errorf("invalid package ID %q", pattern)
			}
			list.exact[normalized] = struct{}{}
		}
	}
	return list, nil
}

func (a *Allowlist) Allows(id string) bool {
	if !ValidID(id) {
		return false
	}
	normalized := strings.ToLower(id)
	if _, ok := a.exact[normalized]; ok {
		return true
	}
	for _, prefix := range a.prefixes {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

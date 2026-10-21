package policy

import (
	"strings"
	"testing"
)

func TestAllowlist(t *testing.T) {
	list, err := New([]string{"Newtonsoft.Json", "Ddth.*", "Partial*"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id      string
		allowed bool
	}{
		{"Newtonsoft.Json", true}, {"NEWTONSOFT.JSON", true},
		{"Newtonsoft.Json.Other", false}, {"Ddth.Core", true},
		{"ddth.core.subpackage", true}, {"Ddth", false}, {"DdthExtra", false},
		{"Partial", true}, {"PartialPackage", true}, {"Other.Partial", false},
		{"", false}, {"../Ddth.Core", false}, {"Ddth..Core", false},
		{"Ddth." + strings.Repeat("x", 96), false}, {"Ddth.\u4e2d\u6587", false},
	} {
		t.Run(test.id, func(t *testing.T) {
			if got := list.Allows(test.id); got != test.allowed {
				t.Fatalf("Allows(%q) = %v, want %v", test.id, got, test.allowed)
			}
		})
	}
	empty, err := New(nil)
	if err != nil || empty.Allows("Newtonsoft.Json") {
		t.Fatal("an empty allowlist must expose no packages")
	}
}

func TestInvalidPatterns(t *testing.T) {
	for _, pattern := range []string{"", "*", " Ddth.*", "Ddth.* ", "Ddth.**", "*.Core", "Ddth.*.Core", "Ddth-.*", "Ddth.-*", "Ddth..*", ".Ddth", "Ddth.", strings.Repeat("x", 101)} {
		t.Run(pattern, func(t *testing.T) {
			if _, err := New([]string{pattern}); err == nil {
				t.Fatalf("accepted invalid pattern %q", pattern)
			}
		})
	}
}

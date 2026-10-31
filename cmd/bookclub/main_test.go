package main

import (
	"slices"
	"testing"
)

func TestParseIDs(t *testing.T) {
	for in, want := range map[string][]int64{
		"":                 nil,
		"  ":               nil,
		"111":              {111},
		"111,222":          {111, 222},
		" 111 , 222 ;333 ": {111, 222, 333},
		"111 222":          {111, 222},
	} {
		got, err := parseIDs(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("parseIDs(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"@anna", "111,abc", "-5", "0"} {
		if _, err := parseIDs(in); err == nil {
			t.Errorf("parseIDs(%q) must fail", in)
		}
	}
}

package main

import (
	"reflect"
	"testing"
)

func TestParseKinds(t *testing.T) {
	got, err := parseKinds("0, 5,1059,30500-30503")
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{0, 5, 1059, 30500, 30501, 30502, 30503}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	for _, bad := range []string{"x", "5-1", "1-x"} {
		if _, err := parseKinds(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

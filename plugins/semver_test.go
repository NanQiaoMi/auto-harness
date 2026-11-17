package plugins

import "testing"

func TestParseVersionAndSort(t *testing.T) {
	valid := []string{"0.0.0", "1.2.3", "10.20.30"}
	for _, value := range valid {
		if _, err := ParseVersion(value); err != nil {
			t.Errorf("ParseVersion(%q): %v", value, err)
		}
	}
	invalid := []string{"1", "1.2", "1.2.3.4", "01.2.3", "1.2.x", "1.2.3-beta"}
	for _, value := range invalid {
		if _, err := ParseVersion(value); err == nil {
			t.Errorf("ParseVersion(%q) succeeded", value)
		}
	}

	sorted, err := sortVersionsDescending([]string{"1.0.0", "10.0.0", "2.1.0", "2.0.9"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0", "2.1.0", "2.0.9", "1.0.0"}
	for i := range want {
		if sorted[i] != want[i] {
			t.Fatalf("sorted = %v, want %v", sorted, want)
		}
	}
}

func TestConstraints(t *testing.T) {
	cases := []struct {
		constraint string
		version    string
		match      bool
	}{
		{"*", "9.9.9", true},
		{"1.2.3", "1.2.3", true},
		{"1.2.3", "1.2.4", false},
		{">=1.0.0 <2.0.0", "1.9.9", true},
		{">=1.0.0 <2.0.0", "2.0.0", false},
		{"^1.2.3", "1.9.0", true},
		{"^1.2.3", "2.0.0", false},
		{"^0.2.3", "0.2.9", true},
		{"^0.2.3", "0.3.0", false},
		{"^0.0.3", "0.0.3", true},
		{"^0.0.3", "0.0.4", false},
		{"~1.2.3", "1.2.9", true},
		{"~1.2.3", "1.3.0", false},
		{"<1.0.0", "0.9.0", true},
	}
	for _, tc := range cases {
		constraint, err := ParseConstraint(tc.constraint)
		if err != nil {
			t.Fatalf("ParseConstraint(%q): %v", tc.constraint, err)
		}
		version, err := ParseVersion(tc.version)
		if err != nil {
			t.Fatal(err)
		}
		if got := constraint.Match(version); got != tc.match {
			t.Errorf("%q matches %s = %v, want %v", tc.constraint, tc.version, got, tc.match)
		}
	}
}

func TestInvalidConstraint(t *testing.T) {
	for _, value := range []string{"=>1.0.0", ">= 1.0.0", "~1", "^1.2"} {
		if _, err := ParseConstraint(value); err == nil {
			t.Errorf("ParseConstraint(%q) succeeded", value)
		}
	}
}

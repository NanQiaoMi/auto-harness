package envsubst

import (
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	lookup := func(name string) (string, bool) {
		values := map[string]string{
			"TOKEN": "abc",
			"EMPTY": "",
		}
		value, ok := values[name]
		return value, ok
	}
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{name: "plain", input: "hello", want: "hello"},
		{name: "value", input: "x-${TOKEN}-y", want: "x-abc-y"},
		{name: "empty is set", input: "${EMPTY:-fallback}", want: ""},
		{name: "fallback", input: "${MISSING:-fallback}", want: "fallback"},
		{name: "escaped", input: "$${TOKEN}", want: "${TOKEN}"},
		{name: "bare dollar", input: "$TOKEN", want: "$TOKEN"},
		{name: "missing", input: "${MISSING}", wantErr: "not set"},
		{name: "unclosed", input: "${TOKEN", wantErr: "unclosed"},
		{name: "invalid name", input: "${1TOKEN}", wantErr: "invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Expand(tc.input, lookup)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Expand = %q, want %q", got, tc.want)
			}
		})
	}
}

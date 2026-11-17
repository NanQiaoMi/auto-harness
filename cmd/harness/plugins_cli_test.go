package main

import "testing"

func TestSplitPluginSpec(t *testing.T) {
	cases := []struct {
		input      string
		name       string
		constraint string
	}{
		{"app", "app", "*"},
		{"app@1.2.3", "app", "1.2.3"},
		{" app @ ^1.0.0 ", "app", "^1.0.0"},
	}
	for _, tc := range cases {
		name, constraint, err := splitPluginSpec(tc.input)
		if err != nil {
			t.Fatalf("splitPluginSpec(%q): %v", tc.input, err)
		}
		if name != tc.name || constraint != tc.constraint {
			t.Errorf("splitPluginSpec(%q) = %q, %q", tc.input, name, constraint)
		}
	}
	for _, value := range []string{"@1.0.0", "app@", "", "  "} {
		if _, _, err := splitPluginSpec(value); err == nil {
			t.Errorf("splitPluginSpec(%q) succeeded", value)
		}
	}
}

func TestPluginCommandRejectsMultipleActions(t *testing.T) {
	isolateHome(t)
	handled, code, err := runPluginCommand(pluginCommandOptions{
		List:   true,
		Verify: true,
	})
	if !handled || code == 0 || err == nil {
		t.Fatalf("handled/code/error = %v/%d/%v", handled, code, err)
	}
}

func TestPluginCommandListAndVerifyEmptyHome(t *testing.T) {
	isolateHome(t)
	handled, code, err := runPluginCommand(pluginCommandOptions{List: true})
	if !handled || code != 0 || err != nil {
		t.Fatalf("list handled/code/error = %v/%d/%v", handled, code, err)
	}
	handled, code, err = runPluginCommand(pluginCommandOptions{Verify: true})
	if !handled || code != 0 || err != nil {
		t.Fatalf("verify handled/code/error = %v/%d/%v", handled, code, err)
	}
}

func TestPluginCommandIgnoresIndexWithoutAction(t *testing.T) {
	handled, code, err := runPluginCommand(pluginCommandOptions{Index: "/not/used"})
	if handled || code != 0 || err != nil {
		t.Fatalf("handled/code/error = %v/%d/%v", handled, code, err)
	}
}

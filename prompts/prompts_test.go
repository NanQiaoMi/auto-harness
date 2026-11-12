package prompts

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestSplitArgs checks the shell-style splitting.
func TestSplitArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"one", []string{"one"}},
		{"one two", []string{"one", "two"}},
		{"  one   two  ", []string{"one", "two"}},
		// A quoted argument is one argument.
		{`"API compatibility"`, []string{"API compatibility"}},
		{`'single quoted'`, []string{"single quoted"}},
		{`a "b c" d`, []string{"a", "b c", "d"}},
		// A quoted empty argument is still an argument.
		{`a "" b`, []string{"a", "", "b"}},
		// A backslash escapes the next character.
		{`a\ b`, []string{"a b"}},
		{`"a\"b"`, []string{`a"b`}},
		// The quotes are removed but the text inside is kept.
		{`"it's"`, []string{"it's"}},
		// A tab separates arguments too.
		{"a\tb", []string{"a", "b"}},
	}
	for _, c := range cases {
		got, err := SplitArgs(c.in)
		if err != nil {
			t.Errorf("SplitArgs(%q): %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitArgs(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestSplitArgsRejectsAnUnclosedQuote checks that a half-quoted
// argument is an error rather than a guess.
func TestSplitArgsRejectsAnUnclosedQuote(t *testing.T) {
	if _, err := SplitArgs(`"unclosed`); err == nil {
		t.Error("the unclosed quote was accepted")
	}
	if _, err := SplitArgs(`'unclosed`); err == nil {
		t.Error("the unclosed quote was accepted")
	}
}

// TestExpand checks the argument substitution.
func TestExpand(t *testing.T) {
	cases := []struct {
		name string
		body string
		args []string
		want string
	}{
		{
			name: "a positional argument",
			body: "Focus on $1.",
			args: []string{"security"},
			want: "Focus on security.",
		},
		{
			name: "a missing positional argument",
			body: "Focus on $1.",
			args: nil,
			want: "Focus on .",
		},
		{
			name: "a two-digit positional argument",
			body: "$10",
			args: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"},
			want: "j",
		},
		{
			name: "every argument",
			body: "Args: $@",
			args: []string{"one", "two"},
			want: "Args: one two",
		},
		{
			name: "every argument, spelled out",
			body: "Args: $ARGUMENTS",
			args: []string{"one", "two"},
			want: "Args: one two",
		},
		{
			name: "a default",
			body: "Focus on ${1:-correctness}.",
			args: nil,
			want: "Focus on correctness.",
		},
		{
			name: "a default that is not used",
			body: "Focus on ${1:-correctness}.",
			args: []string{"security"},
			want: "Focus on security.",
		},
		{
			name: "a default for every argument",
			body: "${@:-none}",
			args: nil,
			want: "none",
		},
		{
			name: "a slice",
			body: "${@:2}",
			args: []string{"one", "two", "three"},
			want: "two three",
		},
		{
			name: "a slice with a length",
			body: "${@:2:1}",
			args: []string{"one", "two", "three"},
			want: "two",
		},
		{
			name: "a slice past the end",
			body: "${@:5}",
			args: []string{"one"},
			want: "",
		},
		{
			name: "a dollar at the end",
			body: "costs $",
			args: nil,
			want: "costs $",
		},
		{
			name: "an unclosed brace",
			body: "a ${1 b",
			args: []string{"x"},
			want: "a ${1 b",
		},
		{
			name: "a literal dollar",
			body: "a $ b",
			args: nil,
			want: "a $ b",
		},
		{
			name: "an unknown expression",
			body: "${HOME}",
			args: nil,
			want: "${HOME}",
		},
		{
			name: "no placeholders",
			body: "just text",
			args: []string{"x"},
			want: "just text",
		},
	}
	for _, c := range cases {
		got := Expand(c.body, c.args)
		if got != c.want {
			t.Errorf("%s: Expand(%q, %v) = %q, want %q", c.name, c.body, c.args, got, c.want)
		}
	}
}

// TestExpandDoesNotExpandTwice checks that a substituted argument is
// not itself expanded.
//
// The model's own output must not be able to turn into a new
// placeholder: a template that says "$1" and is called with "$2" must
// produce "$2", not the second argument.
func TestExpandDoesNotExpandTwice(t *testing.T) {
	got := Expand("value: $1", []string{"$2"})
	if got != "value: $2" {
		t.Errorf("Expand = %q, want %q", got, "value: $2")
	}
}

// TestParseFrontmatter checks the frontmatter parsing.
func TestParseFrontmatter(t *testing.T) {
	content := `---
description: Review the staged changes
argument-hint: "[focus]"
---
Review the staged changes. Focus on ${1:-correctness}.`

	template, err := Parse("review", "review.md", content)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if template.Description != "Review the staged changes" {
		t.Errorf("the description is %q", template.Description)
	}
	if template.ArgumentHint != "[focus]" {
		t.Errorf("the argument hint is %q", template.ArgumentHint)
	}
	if template.Body != "Review the staged changes. Focus on ${1:-correctness}." {
		t.Errorf("the body is %q", template.Body)
	}
}

// TestParseWithoutFrontmatter checks that a plain markdown file is a
// template whose description is its first line.
func TestParseWithoutFrontmatter(t *testing.T) {
	content := "# Review\n\nReview the staged changes."

	template, err := Parse("review", "review.md", content)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if template.Description != "Review" {
		t.Errorf("the description is %q, want %q", template.Description, "Review")
	}
	if template.Body != content {
		t.Errorf("the body is %q", template.Body)
	}
}

// TestParseRejectsAnUnclosedFrontmatter checks that a broken
// frontmatter is an error.
func TestParseRejectsAnUnclosedFrontmatter(t *testing.T) {
	content := "---\ndescription: broken\nthe body"
	if _, err := Parse("x", "x.md", content); err == nil {
		t.Error("the unclosed frontmatter was accepted")
	}
}

// TestParseIgnoresUnknownFields checks that a field this parser does
// not know does not break the template.
func TestParseIgnoresUnknownFields(t *testing.T) {
	content := "---\ndescription: hi\nallowed-tools: bash\n---\nbody"

	template, err := Parse("x", "x.md", content)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if template.Description != "hi" {
		t.Errorf("the description is %q", template.Description)
	}
}

// TestTemplateName checks the file name to command name translation.
func TestTemplateName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"review.md", "review"},
		{"code-review.md", "code-review"},
		{"code_review.md", "code_review"},
		{"review.txt", "review"},
		{"my review.md", ""},
		{".md", ""},
		{"a/b.md", ""},
	}
	for _, c := range cases {
		if got := templateName(c.in); got != c.want {
			t.Errorf("templateName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLoadDir checks that a directory's templates are read in order.
//
// A file that is not a template is skipped: one broken file should not
// take the others down.
func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "review.md", "---\ndescription: Review\n---\nreview the changes")
	write(t, dir, "audit.md", "---\ndescription: Audit\n---\naudit the code")
	write(t, dir, "notes.txt", "not a template")
	write(t, dir, "bad name.md", "not a template either")

	templates, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(templates) != 2 {
		t.Fatalf("templates = %d, want 2", len(templates))
	}
	if templates[0].Name != "audit" || templates[1].Name != "review" {
		t.Errorf("the templates are %q and %q, want audit and review", templates[0].Name, templates[1].Name)
	}
}

// TestLoadDirOfAMissingDirectory checks that a directory that is not
// there is not an error.
func TestLoadDirOfAMissingDirectory(t *testing.T) {
	templates, err := LoadDir(t.TempDir() + "/missing")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(templates) != 0 {
		t.Errorf("templates = %d, want 0", len(templates))
	}
}

// TestRegistryAdd checks that a duplicate name gets a suffix.
func TestRegistryAdd(t *testing.T) {
	registry := NewRegistry()
	first := registry.Add(&Template{Name: "review"})
	second := registry.Add(&Template{Name: "review"})
	if first != "review" {
		t.Errorf("the first name is %q, want %q", first, "review")
	}
	if second != "review:1" {
		t.Errorf("the second name is %q, want %q", second, "review:1")
	}
	if registry.Len() != 2 {
		t.Errorf("the registry holds %d templates, want 2", registry.Len())
	}
}

// TestRegistryRemove checks the removal.
func TestRegistryRemove(t *testing.T) {
	registry := NewRegistry()
	registry.Add(&Template{Name: "review"})
	if !registry.Remove("review") {
		t.Error("the removal failed")
	}
	if registry.Len() != 0 {
		t.Errorf("the registry holds %d templates, want 0", registry.Len())
	}
	if registry.Remove("review") {
		t.Error("removing a template that is not there succeeded")
	}
}

// write writes a file into a directory.
func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

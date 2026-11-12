package frontmatter

import (
	"strings"
	"testing"
)

// TestParseWithoutFrontmatter checks that a plain document is not an
// error.
func TestParseWithoutFrontmatter(t *testing.T) {
	fields, body, err := Parse("# Title\n\nBody text.\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(fields) != 0 {
		t.Errorf("the fields are %v, want none", fields)
	}
	if body != "# Title\n\nBody text." {
		t.Errorf("the body is %q", body)
	}
}

// TestParseReadsTheFields checks the basic mapping.
func TestParseReadsTheFields(t *testing.T) {
	fields, body, err := Parse(`---
name: pdf-tools
description: Extract text from PDFs.
---

# PDF tools

Read references/formats.md first.
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(fields) != 2 {
		t.Fatalf("the fields are %v, want 2", fields)
	}
	name, ok := Get(fields, "name")
	if !ok || name != "pdf-tools" {
		t.Errorf("the name is %q", name)
	}
	description, ok := Get(fields, "description")
	if !ok || description != "Extract text from PDFs." {
		t.Errorf("the description is %q", description)
	}
	if body != "# PDF tools\n\nRead references/formats.md first." {
		t.Errorf("the body is %q", body)
	}
}

// TestParseReadsTheQuotedValues checks that quotes are removed.
func TestParseReadsTheQuotedValues(t *testing.T) {
	fields, _, err := Parse("---\nname: \"pdf-tools\"\ndescription: 'Extract text.'\n---\nbody\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	name, _ := Get(fields, "name")
	if name != "pdf-tools" {
		t.Errorf("the name is %q", name)
	}
	description, _ := Get(fields, "description")
	if description != "Extract text." {
		t.Errorf("the description is %q", description)
	}
}

// TestParseReadsALiteralBlock checks the pipe block scalar.
//
// A description that spans several lines is a common way to write a
// long routing signal, so the block has to keep its line breaks.
func TestParseReadsALiteralBlock(t *testing.T) {
	fields, _, err := Parse(`---
description: |
  This is a multiline description.
  It spans multiple lines.
---

body
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	description, _ := Get(fields, "description")
	if !strings.Contains(description, "This is a multiline description.") {
		t.Errorf("the description is %q", description)
	}
	if !strings.Contains(description, "\n") {
		t.Errorf("the description lost its line breaks: %q", description)
	}
	if !strings.Contains(description, "It spans multiple lines.") {
		t.Errorf("the description is %q", description)
	}
}

// TestParseReadsAFoldedBlock checks the greater-than block scalar.
func TestParseReadsAFoldedBlock(t *testing.T) {
	fields, _, err := Parse(`---
description: >
  This is a folded description.
  It spans multiple lines.
---

body
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	description, _ := Get(fields, "description")
	if !strings.Contains(description, "This is a folded description. It spans multiple lines.") {
		t.Errorf("the description is %q", description)
	}
}

// TestParseStripsTheBlockIndent checks that the block's common
// indentation is removed.
func TestParseStripsTheBlockIndent(t *testing.T) {
	fields, _, err := Parse("---\ndescription: |\n    indented line\n    another line\n---\nbody\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	description, _ := Get(fields, "description")
	if strings.Contains(description, "  ") {
		t.Errorf("the description kept its indentation: %q", description)
	}
	if !strings.Contains(description, "indented line") {
		t.Errorf("the description is %q", description)
	}
}

// TestParseRejectsAnUnclosedBlock checks that a broken frontmatter is
// an error.
//
// A frontmatter that never closes means the file was written wrong, and
// reading the whole document as the body would feed the model the
// metadata as instructions.
func TestParseRejectsAnUnclosedBlock(t *testing.T) {
	if _, _, err := Parse("---\nname: broken\nthe body\n"); err == nil {
		t.Error("the unclosed frontmatter was accepted")
	}
}

// TestParseRejectsANonFieldLine checks that a line that is not a field
// is an error.
func TestParseRejectsANonFieldLine(t *testing.T) {
	if _, _, err := Parse("---\nname: fine\nnot a field\n---\nbody\n"); err == nil {
		t.Error("the malformed line was accepted")
	}
}

// TestParseRejectsACollection checks that a collection value is
// refused.
//
// A half-read collection is worse than a clear failure: the harness
// would carry a value the file never said.
func TestParseRejectsACollection(t *testing.T) {
	if _, _, err := Parse("---\nname: [unclosed\n---\nbody\n"); err == nil {
		t.Error("the collection was accepted")
	}
	if _, _, err := Parse("---\nname: {a: b}\n---\nbody\n"); err == nil {
		t.Error("the mapping was accepted")
	}
}

// TestParseReadsTheEscapes checks the escapes in a quoted value.
func TestParseReadsTheEscapes(t *testing.T) {
	fields, _, err := Parse("---\ndescription: \"a\\tb\\nc\"\n---\nbody\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	description, _ := Get(fields, "description")
	if description != "a\tb\nc" {
		t.Errorf("the description is %q", description)
	}
}

// TestParseNormalizesTheLineEndings checks that CRLF documents read
// the same as LF ones.
func TestParseNormalizesTheLineEndings(t *testing.T) {
	fields, body, err := Parse("---\r\nname: pdf-tools\r\n---\r\n\r\nBody.\r\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	name, _ := Get(fields, "name")
	if name != "pdf-tools" {
		t.Errorf("the name is %q", name)
	}
	if body != "Body." {
		t.Errorf("the body is %q", body)
	}
}

// TestGetIsCaseSensitive checks the field lookup.
//
// YAML keys are case-sensitive, and a file that spells a field
// differently is a file that means something else.
func TestGetIsCaseSensitive(t *testing.T) {
	fields, _, err := Parse("---\nName: pdf-tools\n---\nbody\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := Get(fields, "name"); ok {
		t.Error("the field was found under a different case")
	}
}

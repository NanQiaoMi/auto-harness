package skills

import (
	"fmt"
	"strings"
)

// FormatForPrompt renders the skills for the system prompt.
//
// Only the name, the description and the location go in: the
// instructions stay on disk until the model reads the file. That is
// the whole point of the layer — a library of fifty skills costs fifty
// lines of prompt, not fifty documents.
//
// A skill the model may not invoke is left out entirely: advertising
// a skill the model cannot use would only waste its attention.
//
// The result is empty when there is nothing to advertise, so a run
// without skills has the same prompt as before.
func FormatForPrompt(skills []*Skill) string {
	visible := make([]*Skill, 0, len(skills))
	for _, skill := range skills {
		if skill.DisableModelInvocation {
			continue
		}
		visible = append(visible, skill)
	}
	if len(visible) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\nThe following skills provide specialized instructions for specific tasks.")
	b.WriteString("\nUse the read tool to load a skill's file when the task matches its description.")
	b.WriteString("\nResolve relative paths against the skill's directory (the parent of SKILL.md). Use absolute paths in tool calls.")
	b.WriteString("\n\n<available_skills>")
	for _, skill := range visible {
		b.WriteString("\n  <skill>")
		fmt.Fprintf(&b, "\n    <name>%s</name>", escapeXML(skill.Name))
		fmt.Fprintf(&b, "\n    <description>%s</description>", escapeXML(skill.Description))
		fmt.Fprintf(&b, "\n    <location>%s</location>", escapeXML(skill.Path))
		b.WriteString("\n  </skill>")
	}
	b.WriteString("\n</available_skills>")
	return b.String()
}

// escapeXML escapes the characters that would break the XML block.
//
// A description is free text: it may hold an ampersand or a quote, and
// an unescaped one would end the block early and let the text after it
// read as markup.
func escapeXML(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return replacer.Replace(s)
}

// Block renders a skill's instructions as the block the model reads.
//
// The block names the skill and its location, and says the references
// are relative to the skill's directory. The instructions follow.
//
// It is the form the /skill:name command sends: the model gets the
// skill's full text without having to read the file first.
//
// The block says the user asked for the skill. Without that, a model
// that reads the file on its own sees the disable-model-invocation
// flag and refuses to act — but that flag governs the model's own
// choice of skill, not the user's explicit request.
func (s *Skill) Block() (string, error) {
	body, err := s.Body()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<skill name=%q location=%q>\n", s.Name, s.Path)
	fmt.Fprintf(&b, "References are relative to %s.\n", s.Dir)
	b.WriteString("The user invoked this skill explicitly; follow its instructions.\n\n")
	b.WriteString(body)
	b.WriteString("\n</skill>")
	return b.String(), nil
}

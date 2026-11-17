// Package envsubst expands the deliberately small environment-variable
// syntax used by agent profiles and plugin manifests.
//
// Only ${NAME} and ${NAME:-default} are recognised. Bare $NAME is left
// alone on purpose: profile files frequently contain prompts and shell
// examples, where an accidental $word should stay literal. A missing
// required variable is an error, so configuration mistakes fail at load
// time instead of silently producing an empty credential or path.
package envsubst

import (
	"fmt"
	"os"
	"strings"
)

// Lookup resolves one environment variable.
type Lookup func(name string) (string, bool)

// OS returns a lookup over the current process environment.
func OS() Lookup {
	return os.LookupEnv
}

// Expand expands ${NAME} and ${NAME:-default} with lookup.
//
// $$ is an escaped dollar sign. Every other dollar character is kept
// literally. An unclosed expression, an invalid name, or a required
// variable missing from lookup is an error.
func Expand(value string, lookup Lookup) (string, error) {
	if lookup == nil {
		lookup = OS()
	}
	var b strings.Builder
	for i := 0; i < len(value); {
		if value[i] != '$' {
			b.WriteByte(value[i])
			i++
			continue
		}
		if i+1 < len(value) && value[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if i+1 >= len(value) || value[i+1] != '{' {
			b.WriteByte('$')
			i++
			continue
		}

		end := strings.IndexByte(value[i+2:], '}')
		if end < 0 {
			return "", fmt.Errorf("unclosed environment expression at byte %d", i)
		}
		end += i + 2
		body := value[i+2 : end]
		name, fallback, hasFallback := strings.Cut(body, ":-")
		if !validName(name) {
			return "", fmt.Errorf("invalid environment variable name %q", name)
		}
		resolved, ok := lookup(name)
		if !ok {
			if !hasFallback {
				return "", fmt.Errorf("environment variable %s is not set", name)
			}
			resolved = fallback
		}
		b.WriteString(resolved)
		i = end + 1
	}
	return b.String(), nil
}

// validName accepts shell-style environment names.
func validName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if i == 0 {
			if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
				return false
			}
			continue
		}
		if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

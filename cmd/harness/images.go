package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/NanQiaoMi/auto-harness/llm"
)

// stringSliceFlag collects repeatable string CLI flags (e.g. -image a.png -image b.jpg).
type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("image path cannot be empty")
	}
	*s = append(*s, v)
	return nil
}

func loadCLIImages(paths []string) ([]llm.Image, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	images := make([]llm.Image, 0, len(paths))
	for _, p := range paths {
		img, err := llm.LoadImageFile(p)
		if err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	return images, nil
}

// extractPromptImages scans input for @<path> tokens that reference
// supported image files (PNG, JPEG, WebP, GIF), loads them, and returns
// the cleaned prompt text alongside the loaded images. Non-image
// @tokens (such as @user or @file.go) are preserved verbatim.
func extractPromptImages(input, cwd string) (string, []llm.Image, error) {
	tokens := splitPromptTokens(input)
	var kept []string
	var images []llm.Image
	for _, tok := range tokens {
		if strings.HasPrefix(tok, "@") && len(tok) > 1 {
			rawPath := unquoteToken(tok[1:])
			if llm.ImageMIMEFromExt(filepath.Ext(rawPath)) != "" {
				resolved := rawPath
				if !filepath.IsAbs(resolved) && cwd != "" {
					resolved = filepath.Join(cwd, resolved)
				}
				info, err := os.Stat(resolved)
				if err != nil {
					return "", nil, fmt.Errorf("load image %s: %w", rawPath, err)
				}
				if info.IsDir() {
					return "", nil, fmt.Errorf("load image %s: path is a directory", rawPath)
				}
				img, err := llm.LoadImageFile(resolved)
				if err != nil {
					return "", nil, err
				}
				images = append(images, img)
				continue
			}
		}
		kept = append(kept, tok)
	}
	return strings.Join(kept, " "), images, nil
}

// splitPromptTokens splits on whitespace while keeping @"..." or @'...'
// quoted paths as single tokens.
func splitPromptTokens(s string) []string {
	var tokens []string
	var cur strings.Builder
	var quote byte
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			if cur.Len() == 1 && cur.String() == "@" {
				quote = c
				cur.WriteByte(c)
				continue
			}
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			flush()
			continue
		}
		cur.WriteByte(c)
	}
	flush()
	return tokens
}

func unquoteToken(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

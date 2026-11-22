package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func writeTestPNG(t *testing.T, path string) {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 6, 4))
	im.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, im); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestExtractPromptImages(t *testing.T) {
	dir := t.TempDir()
	writeTestPNG(t, filepath.Join(dir, "shot.png"))
	writeTestPNG(t, filepath.Join(dir, "my dir", "ui bug.png"))

	clean, imgs, err := extractPromptImages(`compare @shot.png and @"my dir/ui bug.png" with @main.go @alice`, dir)
	if err != nil {
		t.Fatalf("extractPromptImages: %v", err)
	}
	if clean != "compare and with @main.go @alice" {
		t.Errorf("clean = %q", clean)
	}
	if len(imgs) != 2 {
		t.Fatalf("imgs = %d, want 2", len(imgs))
	}
	if imgs[0].Width != 6 || imgs[0].Height != 4 || imgs[0].MediaType != "image/png" {
		t.Errorf("img[0] = %+v", imgs[0])
	}

	if _, _, err := extractPromptImages("look at @missing.png", dir); err == nil {
		t.Error("expected error for missing @image file")
	}
}

func TestStringSliceFlagAndLoadCLIImages(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.png")
	writeTestPNG(t, p)

	var fl stringSliceFlag
	if err := fl.Set(""); err == nil {
		t.Error("empty -image flag should fail")
	}
	if err := fl.Set(p); err != nil {
		t.Fatal(err)
	}
	imgs, err := loadCLIImages(fl)
	if err != nil {
		t.Fatalf("loadCLIImages: %v", err)
	}
	if len(imgs) != 1 || imgs[0].MediaType != "image/png" {
		t.Errorf("imgs = %+v", imgs)
	}
}

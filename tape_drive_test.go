package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Fixtures in this file are deliberately synthetic: no real page names, data
// files, templates, addresses, tokens or operator paths. Keep it that way, this
// repository is public.

func TestNormalizeNumbers(t *testing.T) {
	tests := []struct {
		name    string
		in      any
		want    any
		wantInt bool
	}{
		{name: "whole float becomes int64", in: float64(42), want: int64(42), wantInt: true},
		{name: "fraction stays float64", in: 1.5, want: 1.5},
		{name: "zero becomes int64", in: float64(0), want: int64(0), wantInt: true},
		{name: "string untouched", in: "42", want: "42"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := normalize(test.in)
			if got != test.want {
				t.Fatalf("normalize(%v) = %v (%T), want %v", test.in, got, got, test.want)
			}
			if _, isInt := got.(int64); test.wantInt != isInt {
				t.Fatalf("normalize(%v) type = %T, wantInt %v", test.in, got, test.wantInt)
			}
		})
	}
}

func TestNormalizeNested(t *testing.T) {
	in := map[string]any{
		"count": float64(3),
		"rows":  []any{map[string]any{"value": float64(7)}},
	}

	got := normalize(in).(map[string]any)
	if got["count"] != int64(3) {
		t.Fatalf("count = %v (%T), want int64(3)", got["count"], got["count"])
	}

	row := got["rows"].([]any)[0].(map[string]any)
	if row["value"] != int64(7) {
		t.Fatalf("rows[0].value = %v (%T), want int64(7)", row["value"], row["value"])
	}
}

func TestFilenameFitsFirmwareLimit(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef"
	at := time.Unix(1700000000, 0)

	for _, name := range []string{"a", "screen", "a-name-that-is-far-too-long"} {
		got := filename(name, key, at)
		if len(got) > 31 {
			t.Fatalf("filename(%q) = %q, %d chars > 31", name, got, len(got))
		}
		if got[len(got)-10:] != "1700000000" {
			t.Fatalf("filename(%q) = %q, want the unix stamp at the end", name, got)
		}
	}
}

func TestInputKeyChangesWithInputs(t *testing.T) {
	base := inputKey([]byte("<html>a</html>"), []byte(`{}`), 800, 480)

	if base != inputKey([]byte("<html>a</html>"), []byte(`{}`), 800, 480) {
		t.Fatal("same inputs produced different keys")
	}
	if base == inputKey([]byte("<html>b</html>"), []byte(`{}`), 800, 480) {
		t.Fatal("template change did not change the key")
	}
	if base == inputKey([]byte("<html>a</html>"), []byte(`{"x":1}`), 800, 480) {
		t.Fatal("data change did not change the key")
	}
	if base == inputKey([]byte("<html>a</html>"), []byte(`{}`), 800, 481) {
		t.Fatal("size change did not change the key")
	}
}

func TestResolvePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}

	tests := []struct {
		path string
		root string
		want string
	}{
		{path: "page.json", root: "/srv/data", want: "/srv/data/page.json"},
		{path: "/abs/page.json", root: "/srv/data", want: "/abs/page.json"},
		{path: "~/x.json", root: "/srv/data", want: filepath.Join(home, "x.json")},
		{path: "~", root: "/srv/data", want: home},
	}

	for _, test := range tests {
		if got := resolvePath(test.path, test.root); got != test.want {
			t.Fatalf("resolvePath(%q, %q) = %q, want %q", test.path, test.root, got, test.want)
		}
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestLoadPagesDefaults(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeFile(t, dir, "pages.json", `{"pages":[{"name":"alpha"}]}`)

	pages, err := loadPages(manifestPath, "/srv/templates", "/srv/data")
	if err != nil {
		t.Fatalf("loadPages: %v", err)
	}

	if len(pages) != 1 {
		t.Fatalf("got %d pages, want 1", len(pages))
	}
	if pages[0].Template != "/srv/templates/alpha.html.tmpl" {
		t.Fatalf("template = %q, want the name-derived default", pages[0].Template)
	}
	if pages[0].Data != "/srv/data/alpha.json" {
		t.Fatalf("data = %q, want the name-derived default", pages[0].Data)
	}
}

func TestLoadPagesExplicitPaths(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeFile(t, dir, "pages.json",
		`{"pages":[{"name":"beta","template":"shared.html.tmpl","data":"screen-1.json"}]}`)

	pages, err := loadPages(manifestPath, "/srv/templates", "/srv/data")
	if err != nil {
		t.Fatalf("loadPages: %v", err)
	}

	if pages[0].Template != "/srv/templates/shared.html.tmpl" || pages[0].Data != "/srv/data/screen-1.json" {
		t.Fatalf("got %+v, want the explicit paths joined to the roots", pages[0])
	}
}

func TestLoadPagesBareArray(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeFile(t, dir, "pages.json", `[{"name":"alpha"},{"name":"beta"}]`)

	pages, err := loadPages(manifestPath, "/srv/templates", "/srv/data")
	if err != nil {
		t.Fatalf("loadPages: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("got %d pages, want 2", len(pages))
	}
}

func TestLoadPagesRejectsDuplicates(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeFile(t, dir, "pages.json", `{"pages":[{"name":"alpha"},{"name":"alpha"}]}`)

	if _, err := loadPages(manifestPath, "/srv/templates", "/srv/data"); err == nil {
		t.Fatal("duplicate page names were accepted")
	}
}

func TestLoadPagesRejectsEmptyManifest(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeFile(t, dir, "pages.json", `{"pages":[]}`)

	if _, err := loadPages(manifestPath, "/srv/templates", "/srv/data"); err == nil {
		t.Fatal("an empty manifest was accepted")
	}
}

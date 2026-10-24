package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Page is one screen in the rotation: a Go HTML template and the JSON file
// that populates it. Nothing about the payload is known to this program — the
// decoded JSON is handed to the template as-is.
type Page struct {
	Name     string `json:"name"`
	Template string `json:"template"`
	Data     string `json:"data"`
}

type manifest struct {
	Pages []Page `json:"pages"`
}

// loadPages reads the page manifest. Template and data paths are relative to
// the template and data roots passed on the command line; a page that omits
// them falls back to its own name, so {"name": "tasks"} means
// <templates>/tasks.html.tmpl and <data>/tasks.json.
func loadPages(path, templatesDir, dataDir string) ([]Page, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pages: %w", err)
	}

	var file manifest
	if err := json.Unmarshal(raw, &file); err != nil {
		// A bare array is accepted too.
		var list []Page
		if listErr := json.Unmarshal(raw, &list); listErr != nil {
			return nil, fmt.Errorf("%s: want {\"pages\": [...]} or [...]: %w", path, err)
		}
		file.Pages = list
	}

	pages := file.Pages
	if len(pages) == 0 {
		return nil, fmt.Errorf("%s: no pages listed", path)
	}

	seen := make(map[string]bool, len(pages))
	for index := range pages {
		page := &pages[index]

		if page.Name == "" {
			page.Name = fmt.Sprintf("page%d", index+1)
		}
		if seen[page.Name] {
			return nil, fmt.Errorf("%s: duplicate page name %q", path, page.Name)
		}
		seen[page.Name] = true

		if page.Template == "" {
			page.Template = page.Name + ".html.tmpl"
		}
		if page.Data == "" {
			page.Data = page.Name + ".json"
		}

		page.Template = resolvePath(page.Template, templatesDir)
		page.Data = resolvePath(page.Data, dataDir)
	}

	return pages, nil
}

// resolvePath expands a leading ~ and resolves relative paths against root, so
// templates and data can live anywhere without depending on the working
// directory.
func resolvePath(path, root string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}

	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}

	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}

	return filepath.Clean(path)
}

// findChrome looks for a browser able to screenshot a local HTML file.
func findChrome() string {
	candidates := []string{
		"google-chrome",
		"google-chrome-stable",
		"chromium",
		"chromium-browser",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
	}

	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}

	return ""
}

// portSuffix turns "0.0.0.0:2300" into ":2300", for the default base URL.
func portSuffix(addr string) string {
	_, port, found := strings.Cut(addr, ":")
	if !found || port == "" {
		return ""
	}
	return ":" + port
}

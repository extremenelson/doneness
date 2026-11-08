package scaffold

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// TestRenderRules pins render's contract on a synthetic template tree:
// __name__ in paths, .tmpl stripped, {{.Field}} substituted, dotfiles kept,
// .sh files executable, everything else 0644.
func TestRenderRules(t *testing.T) {
	fsys := fstest.MapFS{
		"templates/.hidden.tmpl":              {Data: []byte("dot {{.Name}}\n")},
		"templates/bin/run.sh.tmpl":           {Data: []byte("#!/bin/sh\necho {{.Module}}\n")},
		"templates/cmd/__name__/main.go.tmpl": {Data: []byte("package main // {{.Name}}\n")},
		"templates/docs/plain.md.tmpl":        {Data: []byte("plain\n")},
	}
	dest := t.TempDir()
	cfg := Config{Name: "demo", Module: "example.com/demo"}
	if err := render(fsys, cfg, dest); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		path, body string
		mode       os.FileMode
	}{
		{".hidden", "dot demo\n", 0o644},
		{"bin/run.sh", "#!/bin/sh\necho example.com/demo\n", 0o755},
		{"cmd/demo/main.go", "package main // demo\n", 0o644},
		{"docs/plain.md", "plain\n", 0o644},
	}
	for _, tt := range tests {
		p := filepath.Join(dest, tt.path)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("%s: %v", tt.path, err)
			continue
		}
		if string(b) != tt.body {
			t.Errorf("%s = %q, want %q", tt.path, b, tt.body)
		}
		fi, _ := os.Stat(p)
		if fi.Mode().Perm() != tt.mode {
			t.Errorf("%s mode = %o, want %o", tt.path, fi.Mode().Perm(), tt.mode)
		}
	}
}

// TestRenderRefusesToOverwrite: render never replaces an existing file.
func TestRenderRefusesToOverwrite(t *testing.T) {
	fsys := fstest.MapFS{"templates/a.txt.tmpl": {Data: []byte("new\n")}}
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "a.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := render(fsys, Config{Name: "demo"}, dest); err == nil {
		t.Fatal("render overwrote an existing file")
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "a.txt")); string(b) != "mine\n" {
		t.Errorf("existing file changed to %q", b)
	}
}

// TestEmbeddedTemplatesRender: every shipped template parses and executes.
func TestEmbeddedTemplatesRender(t *testing.T) {
	cfg := Config{Name: "demo", Module: "example.com/demo", HookCommandJSON: `"true"`, HookTimeoutSeconds: 1800}
	if err := render(templates, cfg, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

package adopt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func files(t *testing.T, root string, fs map[string]string) {
	t.Helper()
	for name, body := range fs {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func detect(t *testing.T, fs map[string]string) Detection {
	t.Helper()
	root := t.TempDir()
	files(t, root, fs)
	d, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func first(d Detection) string {
	if len(d.Candidates) == 0 {
		return ""
	}
	return d.Candidates[0].Command
}

func excluded(d Detection) string {
	var s []string
	for _, e := range d.Excluded {
		s = append(s, e.Name+": "+e.Reason)
	}
	return strings.Join(s, "; ")
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		want     string // first candidate
		excluded string // substring of the exclusions, "" for none
	}{
		{name: "Makefile aggregate target", files: map[string]string{
			"Makefile": "check: lint test\n\nlint:\n\tgolangci-lint run\n\ntest:\n\tgo test ./...\n",
		}, want: "make check"},
		{name: "ci beats verify only by order: check, ci, verify", files: map[string]string{
			"Makefile": "verify:\n\t./verify.sh\nci:\n\t./ci.sh\n",
		}, want: "make ci"},
		{name: "Makefile composes lint, typecheck, test", files: map[string]string{
			"Makefile": "test:\n\tgo test ./...\nlint:\n\tgolangci-lint run\nbuild:\n\tgo build\n",
		}, want: "make lint && make test"},
		{name: "aggregate that formats is excluded, components composed", files: map[string]string{
			"Makefile": "check: fmt tidy lint test ## all\n\nfmt:\n\tgolangci-lint fmt\n\ntidy:\n\tgo mod tidy\n\nlint:\n\tgolangci-lint run\n\ntest:\n\tgo test -race ./...\n",
		}, want: "make lint && make test", excluded: "Makefile target check: excluded: mutates (fmt without a check flag) (via fmt)"},
		{name: "check-only formatter is fine", files: map[string]string{
			"Makefile": "check:\n\tgolangci-lint fmt --diff\n\tgo mod tidy -diff\n\tgo test ./...\n",
		}, want: "make check"},
		{name: "--fix in a component", files: map[string]string{
			"Makefile": "lint:\n\teslint --fix .\ntest:\n\tnpm test\n",
		}, want: "make test", excluded: "Makefile target lint: excluded: mutates (--fix)"},
		{name: "variables and special targets ignored", files: map[string]string{
			"Makefile": "BIN := app\nCHECK ?= x\n.PHONY: check\ncheck:\n\t@echo $(BIN)\n",
		}, want: "make check"},
		{name: "justfile", files: map[string]string{
			"justfile": "set shell := [\"bash\", \"-c\"]\n\nlint:\n    cargo clippy\n\ntest *args:\n    cargo test {{args}}\n",
		}, want: "just lint && just test"},
		{name: "justfile aggregate with deps", files: map[string]string{
			"justfile": "check: lint\n    echo ok\nlint:\n    cargo fmt\n",
		}, want: "", excluded: "justfile recipe check: excluded: mutates (fmt without a check flag) (via lint)"},
		{name: "package.json npm default", files: map[string]string{
			"package.json": `{"scripts":{"lint":"eslint .","test":"vitest run","build":"tsc"}}`,
		}, want: "npm run lint && npm run test"},
		{name: "package.json pnpm", files: map[string]string{
			"package.json": `{"scripts":{"check":"tsc --noEmit && vitest run"}}`, "pnpm-lock.yaml": "",
		}, want: "pnpm run check"},
		{name: "package.json yarn with typecheck", files: map[string]string{
			"package.json": `{"scripts":{"typecheck":"tsc --noEmit","test":"jest"}}`, "yarn.lock": "",
		}, want: "yarn run typecheck && yarn run test"},
		{name: "package.json bun", files: map[string]string{
			"package.json": `{"scripts":{"test":"bun test"}}`, "bun.lockb": "",
		}, want: "bun run test"},
		{name: "package.json writer excluded", files: map[string]string{
			"package.json": `{"scripts":{"lint":"prettier --write . && eslint .","test":"jest"}}`,
		}, want: "npm run test", excluded: "package.json script lint: excluded: mutates (--write)"},
		{name: "Cargo only", files: map[string]string{"Cargo.toml": "[package]\n"},
			want: "cargo clippy -- -D warnings && cargo test"},
		{name: "go.mod only", files: map[string]string{"go.mod": "module x\n"},
			want: "go vet ./... && go test ./..."},
		{name: "build files rank before the toolchain", files: map[string]string{
			"go.mod": "module x\n", "Makefile": "test:\n\tgo test ./...\n",
		}, want: "make test"},
		{name: "nothing found", files: map[string]string{"README.md": "hi\n"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := detect(t, tt.files)
			if got := first(d); got != tt.want {
				t.Errorf("first candidate = %q, want %q (all: %+v)", got, tt.want, d.Candidates)
			}
			got := excluded(d)
			if tt.excluded == "" && got != "" {
				t.Errorf("unexpected exclusions: %s", got)
			}
			if tt.excluded != "" && !strings.Contains(got, tt.excluded) {
				t.Errorf("exclusions = %q, want containing %q", got, tt.excluded)
			}
		})
	}
}

func TestMutates(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"golangci-lint fmt", true},
		{"golangci-lint fmt --diff", false},
		{"go fmt ./...", true},
		{"gofmt -l .", false},
		{"gofmt -w .", true},
		{"go mod tidy", true},
		{"go mod tidy -diff", false},
		{"cargo fmt", true},
		{"cargo fmt --check", false},
		{"prettier --check .", false},
		{"prettier --write .", true},
		{"black .", true},
		{"black --check .", false},
		{"eslint --fix .", true},
		{"ruff format", true},
		{"go test -run TestFormat ./...", false},
		{"go vet ./...", false},
		{"./scripts/format.sh", false},
	}
	for _, tt := range tests {
		if _, got := mutates(tt.line); got != tt.want {
			t.Errorf("mutates(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

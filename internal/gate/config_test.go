package gate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRaw(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	for _, cmd := range []string{
		"make check",
		"go vet ./... && go test ./...",
		`pnpm run lint && pnpm run "test:unit"`,
		"echo 'quotes' \\ and\ttabs",
	} {
		t.Run(cmd, func(t *testing.T) {
			dir := t.TempDir()
			want := Config{Gate: Gate{Command: cmd}}
			if err := WriteConfig(dir, want); err != nil {
				t.Fatal(err)
			}
			got, err := LoadConfig(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("LoadConfig(WriteConfig(c)) = %+v, want %+v", got, want)
			}
		})
	}
}

func TestWriteConfigFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteConfig(dir, Config{Gate: Gate{Command: "make check"}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ConfigFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"gate\": {\n    \"command\": \"make check\"\n  }\n}\n"; string(b) != want {
		t.Errorf("file =\n%s\nwant\n%s", b, want)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o, want 644", fi.Mode().Perm())
	}

	// Overwrite in place and leave no temp files behind.
	if err := WriteConfig(dir, Config{Gate: Gate{Command: "make test"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadConfig(dir); got.Gate.Command != "make test" {
		t.Errorf("after overwrite command = %q", got.Gate.Command)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dir has %d entries, want only %s", len(entries), ConfigFile)
	}
}

func TestWriteConfigRejectsEmptyCommand(t *testing.T) {
	dir := t.TempDir()
	if err := WriteConfig(dir, Config{Gate: Gate{Command: "  \t"}}); !errors.Is(err, ErrEmptyCommand) {
		t.Fatalf("err = %v, want ErrEmptyCommand", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ConfigFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("invalid config was written (stat err = %v)", err)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    *string // nil: no file
		wantIs  error
		wantMsg string
	}{
		{name: "missing file", wantIs: ErrNoConfig},
		{name: "empty file", body: ptr(""), wantMsg: "EOF"},
		{name: "not json", body: ptr("command = make check"), wantMsg: "invalid character"},
		{name: "no gate section", body: ptr("{}"), wantIs: ErrEmptyCommand},
		{name: "blank command", body: ptr(`{"gate":{"command":"   "}}`), wantIs: ErrEmptyCommand},
		{name: "misspelled key", body: ptr(`{"gate":{"comand":"make check"}}`), wantMsg: `unknown field "comand"`},
		{name: "unknown top-level key", body: ptr(`{"gate":{"command":"x"},"slots":2}`), wantMsg: `unknown field "slots"`},
		{name: "wrong type", body: ptr(`{"gate":{"command":["make","check"]}}`), wantMsg: "cannot unmarshal"},
		{name: "trailing data", body: ptr(`{"gate":{"command":"x"}} {}`), wantMsg: "unexpected data after"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.body != nil {
				writeRaw(t, dir, *tt.body)
			}
			_, err := LoadConfig(dir)
			if err == nil {
				t.Fatal("LoadConfig succeeded, want error")
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("err = %v, want errors.Is %v", err, tt.wantIs)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %q, want containing %q", err, tt.wantMsg)
			}
		})
	}
}

func TestLoadConfigAcceptsHandFormatting(t *testing.T) {
	dir := t.TempDir()
	writeRaw(t, dir, "\n{ \"gate\" :\n\t{\"command\":\"make check\"}   }\n\n")
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Gate.Command != "make check" {
		t.Errorf("command = %q", c.Gate.Command)
	}
}

func TestConfigHash(t *testing.T) {
	a := Config{Gate: Gate{Command: "make check"}}
	if a.Hash() != (Config{Gate: Gate{Command: "make check"}}).Hash() {
		t.Error("equal configs hash differently")
	}
	if a.Hash() == (Config{Gate: Gate{Command: "make test"}}).Hash() {
		t.Error("different commands hash the same")
	}
	if len(a.Hash()) != 64 {
		t.Errorf("hash %q is not 64 hex chars", a.Hash())
	}

	// Reformatting the file must not change the hash (no false "config drift").
	dir := t.TempDir()
	writeRaw(t, dir, "{\n\n   \"gate\": { \"command\": \"make check\" }\n}")
	loaded, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Hash() != a.Hash() {
		t.Error("hand-formatted file hashes differently from the canonical config")
	}
}

func ptr(s string) *string { return &s }

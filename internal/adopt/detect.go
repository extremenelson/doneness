// Package adopt retrofits an existing repository with an enforced
// definition of done: it detects the repository's own check command, writes
// the gate config, installs the Claude Code Stop hook and an agent-rules
// block, and (with Apply) proves the gate goes green once.
package adopt

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Candidate is a check command found in the repository.
type Candidate struct {
	Command string // what the gate would run
	Source  string // where it came from, e.g. "Makefile target check"
}

// Exclusion is a target or script that was left out of the gate, and why.
type Exclusion struct {
	Name   string // e.g. "Makefile target fmt"
	Reason string
}

// Detection is the result of looking for check commands.
type Detection struct {
	Candidates []Candidate // best first
	Excluded   []Exclusion
}

// aggregateNames are target names that conventionally run every check;
// componentNames are composed (in this order) when no aggregate exists.
// Format targets count only when they verify (the writer check decides).
var (
	aggregateNames = []string{"check", "ci", "verify"}
	componentNames = []string{
		"fmt", "format", "fmt-check", "format-check", "check-fmt", "check-format", "fmt:check", "format:check",
		"lint", "vet", "typecheck", "test",
	}
)

// toolchainTest is the language's standard test command, used in place of a
// build file's own `test` target when that target is missing or would
// rewrite files.
type toolchainTest struct {
	command, source string
}

// Detect finds candidate check commands in the repository at root, from the
// repository's own build files first (Makefile, justfile, package.json),
// then from the language toolchain (Cargo, Go). Targets whose recipes
// rewrite files are excluded: the gate must only verify.
func Detect(root string) (Detection, error) {
	var d Detection
	var tc toolchainTest
	switch {
	case exists(filepath.Join(root, "go.mod")):
		tc = toolchainTest{"go test ./...", "go.mod"}
	case exists(filepath.Join(root, "Cargo.toml")):
		tc = toolchainTest{"cargo test", "Cargo.toml"}
	}
	for _, src := range []func(string, toolchainTest, *Detection) error{fromMakefile, fromJustfile, fromPackageJSON} {
		if err := src(root, tc, &d); err != nil {
			return d, err
		}
	}
	if exists(filepath.Join(root, "Cargo.toml")) {
		d.Candidates = append(d.Candidates, rustCandidate(root))
	}
	if exists(filepath.Join(root, "go.mod")) {
		d.Candidates = append(d.Candidates, Candidate{Command: "go vet ./... && go test ./...", Source: "go.mod"})
	}
	return d, nil
}

// rustCandidate is `cargo test`, preceded by the formatting and clippy
// checks this repository's CI already runs (with its -D warnings choice), so
// the gate is not stricter than the project's own standard.
func rustCandidate(root string) Candidate {
	var parts, from []string
	lines := ciLines(root)
	if mentions(lines, "cargo fmt") {
		parts = append(parts, "cargo fmt --all --check")
		from = append(from, "cargo fmt")
	}
	if mentions(lines, "clippy") {
		clippy := "cargo clippy"
		for _, l := range lines {
			if strings.Contains(l, "clippy") && (strings.Contains(l, "-D warnings") || strings.Contains(l, "-Dwarnings")) {
				clippy += " -- -D warnings"
				break
			}
		}
		parts = append(parts, clippy)
		from = append(from, "clippy")
	}
	parts = append(parts, "cargo test")
	source := "Cargo.toml"
	if len(from) > 0 {
		them := "them"
		if len(from) == 1 {
			them = "it"
		}
		source += "; " + strings.Join(from, " and ") + " because this repo's CI runs " + them
	}
	return Candidate{Command: strings.Join(parts, " && "), Source: source}
}

// ciLines returns the lines of the repository's CI configuration files.
func ciLines(root string) []string {
	var paths []string
	for _, pattern := range []string{".github/workflows/*.yml", ".github/workflows/*.yaml"} {
		m, _ := filepath.Glob(filepath.Join(root, pattern))
		paths = append(paths, m...)
	}
	paths = append(paths, filepath.Join(root, ".gitlab-ci.yml"), filepath.Join(root, ".circleci", "config.yml"))
	var lines []string
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			lines = append(lines, strings.Split(string(b), "\n")...)
		}
	}
	return lines
}

func mentions(lines []string, word string) bool {
	for _, l := range lines {
		if strings.Contains(l, word) {
			return true
		}
	}
	return false
}

// target is a named recipe: its prerequisites and command lines.
type target struct {
	prereqs []string
	body    []string
}

// choose picks an aggregate target if one exists and does not mutate,
// otherwise composes the non-mutating component targets. run formats the
// invocation of one target (e.g. "make lint").
func choose(kind string, targets map[string]target, run func(string) string, tc toolchainTest, d *Detection) {
	mut := func(name string) (string, bool) { return mutation(targets, name, map[string]bool{}) }
	for _, name := range aggregateNames {
		if _, ok := targets[name]; !ok {
			continue
		}
		if why, bad := mut(name); bad {
			d.Excluded = append(d.Excluded, Exclusion{Name: kind + " " + name, Reason: why})
			continue
		}
		d.Candidates = append(d.Candidates, Candidate{Command: run(name), Source: kind + " " + name})
		return
	}
	var parts, used []string
	for _, name := range componentNames {
		if _, ok := targets[name]; !ok {
			continue
		}
		if why, bad := mut(name); bad {
			d.Excluded = append(d.Excluded, Exclusion{Name: kind + " " + name, Reason: why})
			continue
		}
		parts = append(parts, run(name))
		used = append(used, name)
	}
	if len(parts) == 0 {
		return
	}
	source := kind + "s " + strings.Join(used, ", ")
	if !slices.Contains(used, "test") && tc.command != "" {
		// The repo's own test target is missing or would rewrite files;
		// a gate with no tests is weak, so use the language's test runner.
		parts = append(parts, tc.command)
		source += fmt.Sprintf(" + `%s` (%s) for tests", tc.command, tc.source)
	}
	d.Candidates = append(d.Candidates, Candidate{Command: strings.Join(parts, " && "), Source: source})
}

// mutation reports whether target name (or any prerequisite, recursively)
// has a command that rewrites files.
func mutation(targets map[string]target, name string, seen map[string]bool) (string, bool) {
	if seen[name] {
		return "", false
	}
	seen[name] = true
	t := targets[name]
	for _, line := range t.body {
		if why, bad := mutates(line); bad {
			return why, true
		}
	}
	for _, p := range t.prereqs {
		if why, bad := mutation(targets, p, seen); bad {
			return fmt.Sprintf("%s (via %s)", why, p), true
		}
	}
	return "", false
}

var (
	writeFlag  = regexp.MustCompile(`(^|\s)(--fix|--write|-w)(\s|=|$)`)
	formatWord = regexp.MustCompile(`(^|[\s/])(fmt|format|gofmt|gofumpt|goimports|black|prettier)(\s|$)`)
	tidyWord   = regexp.MustCompile(`(^|\s)tidy(\s|$)`)
	checkFlag  = regexp.MustCompile(`(^|\s)(--check|--diff|-diff|-d|-l|--list-different|--dry-run)(\s|=|$)`)

	// Commands that rewrite tracked files whatever their flags: go.mod and
	// go.sum, vendored or generated code, lockfiles.
	rewriter = regexp.MustCompile(`(^|[\s;&|(])(go\s+get|go\s+generate|go\s+mod\s+vendor|cargo\s+update)(\s|$)`)
	// Package-manager installs rewrite the lockfile unless frozen.
	pkgInstall = regexp.MustCompile(`(^|[\s;&|(])(npm|pnpm|yarn|bun)\s+(install|i|add|update|upgrade|up)(\s|$)`)
	frozenFlag = regexp.MustCompile(`--frozen-lockfile|--immutable|--ci(\s|$)`)
)

// mutates reports whether a command line rewrites files: an explicit write
// flag (--fix, --write, -w), or a formatter or `tidy` run without a
// check-only flag.
func mutates(line string) (string, bool) {
	l := strings.TrimSpace(line)
	if m := rewriter.FindStringSubmatch(l); m != nil {
		return "excluded: may rewrite tracked files (" + strings.Join(strings.Fields(m[2]), " ") + ")", true
	}
	if m := pkgInstall.FindStringSubmatch(l); m != nil && !frozenFlag.MatchString(l) {
		return "excluded: may rewrite the lockfile (" + m[2] + " " + m[3] + " without a frozen-lockfile flag)", true
	}
	if m := writeFlag.FindStringSubmatch(l); m != nil {
		return "excluded: mutates (" + m[2] + ")", true
	}
	if checkFlag.MatchString(l) {
		return "", false
	}
	if m := formatWord.FindStringSubmatch(l); m != nil {
		return "excluded: mutates (" + m[2] + " without a check flag)", true
	}
	if tidyWord.MatchString(l) {
		return "excluded: mutates (tidy without -diff)", true
	}
	return "", false
}

// makeRule matches a Makefile rule line "name [name...]: prereqs", but not
// variable assignments (":=", "::=") or special targets.
var makeRule = regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9_.\- ]*?)\s*:([^=].*)?$`)

func fromMakefile(root string, tc toolchainTest, d *Detection) error {
	path := ""
	for _, name := range []string{"GNUmakefile", "makefile", "Makefile"} {
		if p := filepath.Join(root, name); exists(p) {
			path = p
			break
		}
	}
	if path == "" {
		return nil
	}
	targets, err := parseRecipes(path, makeRule, func(line string) bool { return strings.HasPrefix(line, "\t") })
	if err != nil {
		return err
	}
	choose("Makefile target", targets, func(t string) string { return "make " + t }, tc, d)
	return nil
}

// justRecipe matches a justfile recipe header "name args...: deps".
var justRecipe = regexp.MustCompile(`^@?([A-Za-z0-9_][A-Za-z0-9_\-]*)(?:\s+[^:=]*)?\s*:([^=].*)?$`)

func fromJustfile(root string, tc toolchainTest, d *Detection) error {
	path := ""
	for _, name := range []string{"justfile", "Justfile", ".justfile"} {
		if p := filepath.Join(root, name); exists(p) {
			path = p
			break
		}
	}
	if path == "" {
		return nil
	}
	targets, err := parseRecipes(path, justRecipe, func(line string) bool {
		return strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
	})
	if err != nil {
		return err
	}
	choose("justfile recipe", targets, func(t string) string { return "just " + t }, tc, d)
	return nil
}

// parseRecipes reads rule headers (header) and their command lines (lines
// for which isBody holds) from a Makefile-like file.
func parseRecipes(path string, header *regexp.Regexp, isBody func(string) bool) (map[string]target, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	targets := map[string]target{}
	var current []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if isBody(line) && strings.TrimSpace(line) != "" {
			for _, name := range current {
				t := targets[name]
				t.body = append(t.body, strings.TrimSpace(line))
				targets[name] = t
			}
			continue
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		m := header.FindStringSubmatch(line)
		if m == nil || strings.HasPrefix(m[1], ".") {
			current = nil
			continue
		}
		deps := m[2]
		if i := strings.Index(deps, "##"); i >= 0 { // "## help text" convention
			deps = deps[:i]
		}
		if i := strings.Index(deps, ";"); i >= 0 { // "target: deps ; recipe"
			deps = deps[:i]
		}
		current = strings.Fields(m[1])
		for _, name := range current {
			t := targets[name]
			t.prereqs = append(t.prereqs, strings.Fields(deps)...)
			targets[name] = t
		}
	}
	return targets, sc.Err()
}

// packageManager picks the package manager from the lockfile, defaulting
// to npm.
func packageManager(root string) string {
	switch {
	case exists(filepath.Join(root, "pnpm-lock.yaml")):
		return "pnpm"
	case exists(filepath.Join(root, "yarn.lock")):
		return "yarn"
	case exists(filepath.Join(root, "bun.lock")), exists(filepath.Join(root, "bun.lockb")):
		return "bun"
	default:
		return "npm"
	}
}

func fromPackageJSON(root string, tc toolchainTest, d *Detection) error {
	b, err := os.ReadFile(filepath.Join(root, "package.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return fmt.Errorf("package.json: %w", err)
	}
	targets := map[string]target{}
	for name, script := range pkg.Scripts {
		targets[name] = target{body: []string{script}}
	}
	pm := packageManager(root)
	choose("package.json script", targets, func(s string) string { return pm + " run " + s }, tc, d)
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

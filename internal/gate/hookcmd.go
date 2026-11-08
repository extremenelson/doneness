package gate

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// BinaryName is the command name the hook invokes when it relies on PATH.
const BinaryName = "newproj"

const (
	// MinHookTimeout is the shortest Stop-hook timeout a gate accepts.
	MinHookTimeout = 60 * time.Second
	// budgetMargin is how long before the hook's timeout the gate gives up
	// on its own, so Claude Code never kills it (a killed hook lets the
	// stop through silently).
	budgetMargin = 30 * time.Second
)

// HookCommand returns the shell command for a Claude Code Stop hook that
// runs the gate, for a hook whose "timeout" is hookTimeout.
//
// bin is the gate binary's absolute path, for personal settings files
// (.claude/settings.local.json), or "" to look BinaryName up on PATH, for
// committed settings files shared with people who may not have it installed.
//
// When the binary is missing the hook does not block: it lets the stop
// through and shows the user a warning, because blocking would trap the
// agent in a loop that nothing could end.
func HookCommand(bin string, hookTimeout time.Duration) (string, error) {
	if hookTimeout < MinHookTimeout {
		return "", fmt.Errorf("hook timeout %v is below the %v minimum", hookTimeout, MinHookTimeout)
	}
	budget := int((hookTimeout - budgetMargin) / time.Second)

	var check, invoke, missing string
	switch {
	case bin == "":
		check = "command -v " + BinaryName + " >/dev/null 2>&1"
		invoke = BinaryName
		missing = BinaryName + " is not on PATH"
	case filepath.IsAbs(bin):
		check = "[ -x " + shellQuote(bin) + " ]"
		invoke = shellQuote(bin)
		missing = bin + " is missing"
	default:
		return "", fmt.Errorf("gate binary path %q is not absolute", bin)
	}

	notice, err := json.Marshal(map[string]string{
		"systemMessage": "done-gate NOT enforced: " + missing,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s || { printf '%%s\\n' %s; exit 0; }; %s gate --hook --budget %ds",
		check, shellQuote(string(notice)), invoke, budget), nil
}

// hookBinaryRE matches a gate invocation at the start of a shell command or
// after a command separator, capturing the binary: BinaryName on PATH, or
// any path (single-quoted as HookCommand writes it, or bare) whose last
// element is BinaryName, followed by `gate --hook`.
var hookBinaryRE = regexp.MustCompile(
	`(?:^|[;&|]\s*)('(?:[^']|'\\'')*/` + BinaryName + `'|(?:\S*/)?` + BinaryName + `)\s+gate\s+--hook(?:\s|$)`)

// HookBinary returns the gate binary a hook command invokes: an absolute
// path, or "" when it looks BinaryName up on PATH. ok is false when cmd is
// not a gate hook.
func HookBinary(cmd string) (path string, ok bool) {
	m := hookBinaryRE.FindStringSubmatch(strings.TrimSpace(cmd))
	if m == nil {
		return "", false
	}
	bin := m[1]
	if strings.HasPrefix(bin, "'") {
		bin = strings.ReplaceAll(bin[1:len(bin)-1], `'\''`, "'")
	}
	if bin == BinaryName {
		return "", true
	}
	return bin, true
}

// IsGateHook reports whether a hook command runs the gate. It recognizes
// every command HookCommand produces and a hand-written
// `newproj gate --hook`.
func IsGateHook(cmd string) bool {
	_, ok := HookBinary(cmd)
	return ok
}

// shellQuote quotes s for POSIX sh: it wraps s in single quotes and, for
// each single quote inside s, closes the quoting, emits an escaped quote and
// reopens it.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

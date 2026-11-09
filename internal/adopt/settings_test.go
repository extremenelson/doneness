package adopt

import (
	"encoding/json"
	"strings"
	"testing"
)

var testHook = hookEntry{Type: "command", Command: "doneness hook --budget 1770s", Timeout: 1800}

// stopCommands decodes a settings file and returns every Stop hook command.
func stopCommands(t *testing.T, b []byte) []string {
	t.Helper()
	var s struct {
		Hooks struct {
			Stop []struct {
				Hooks []hookEntry `json:"hooks"`
			} `json:"Stop"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, b)
	}
	var cmds []string
	for _, g := range s.Hooks.Stop {
		for _, h := range g.Hooks {
			cmds = append(cmds, h.Command)
		}
	}
	return cmds
}

func TestMergeSettingsLocal(t *testing.T) {
	tests := []struct {
		name string
		old  string // "" = no file
		want []string
	}{
		{name: "new file", want: []string{testHook.Command}},
		{name: "keeps other settings and hooks", old: `{"model":"opus","hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}],"PreToolUse":[]}}`,
			want: []string{"say done", testHook.Command}},
		{name: "replaces an existing gate hook", old: `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/old/doneness hook --budget 30s","timeout":60}]}]}}`,
			want: []string{testHook.Command}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var old []byte
			if tt.old != "" {
				old = []byte(tt.old)
			}
			out, _, err := mergeSettings(old, testHook, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := stopCommands(t, out); strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("Stop commands = %q, want %q", got, tt.want)
			}
			if strings.Contains(string(out), `&`) || strings.Contains(string(out), `>`) {
				t.Errorf("HTML-escaped output:\n%s", out)
			}
			if tt.old != "" && strings.Contains(tt.old, `"model"`) && !strings.Contains(string(out), `"model": "opus"`) {
				t.Errorf("other settings dropped:\n%s", out)
			}
			// Idempotent: merging again changes nothing.
			again, _, err := mergeSettings(out, testHook, false)
			if err != nil || string(again) != string(out) {
				t.Errorf("second merge changed the file:\n%s\nvs\n%s", out, again)
			}
		})
	}
}

func TestMergeSettingsMinimalPatch(t *testing.T) {
	tests := []struct {
		name, old, want string
	}{
		{
			name: "append to a Stop array, other bytes untouched",
			old: `{
    "permissions": {"allow": ["Bash(make *)"]},
    "hooks": {
        "Stop": [
            {"hooks": [{"type": "command", "command": "say done"}]}
        ]
    }
}
`,
			want: `{
    "permissions": {"allow": ["Bash(make *)"]},
    "hooks": {
        "Stop": [
            {"hooks": [{"type": "command", "command": "say done"}]},
            {"hooks": [{"type": "command", "command": "doneness hook --budget 1770s", "timeout": 1800}]}
        ]
    }
}
`,
		},
		{
			name: "add Stop to an existing hooks object",
			old: `{
  "hooks": {
    "PreToolUse": []
  }
}
`,
			want: `{
  "hooks": {
    "PreToolUse": [],
    "Stop": [{"hooks": [{"type": "command", "command": "doneness hook --budget 1770s", "timeout": 1800}]}]
  }
}
`,
		},
		{
			name: "empty Stop array",
			old: `{
  "hooks": {
    "Stop": []
  }
}
`,
			want: `{
  "hooks": {
    "Stop": [
      {"hooks": [{"type": "command", "command": "doneness hook --budget 1770s", "timeout": 1800}]}
    ]
  }
}
`,
		},
		{
			name: "replace an existing gate hook in place",
			old: `{
	"hooks": {
		"Stop": [
			{
				"hooks": [
					{ "type": "command", "command": "'/old/doneness' hook --budget 30s", "timeout": 60 }
				]
			}
		]
	}
}
`,
			want: `{
	"hooks": {
		"Stop": [
			{
				"hooks": [
					{"type": "command", "command": "doneness hook --budget 1770s", "timeout": 1800}
				]
			}
		]
	}
}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, note, err := mergeSettings([]byte(tt.old), testHook, true)
			if err != nil {
				t.Fatal(err)
			}
			if note != "" {
				t.Errorf("unexpected fallback: %s", note)
			}
			if string(out) != tt.want {
				t.Errorf("patched file:\n%s\nwant:\n%s", out, tt.want)
			}
			again, note, err := mergeSettings(out, testHook, true)
			if err != nil || note != "" || string(again) != string(out) {
				t.Errorf("second patch changed the file (note %q):\n%s", note, again)
			}
		})
	}
}

func TestMergeSettingsMinimalFallsBack(t *testing.T) {
	out, note, err := mergeSettings([]byte(`{"model": "opus"}`), testHook, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "rewrote the whole file") {
		t.Errorf("note = %q, want a rewrite notice", note)
	}
	if got := stopCommands(t, out); len(got) != 1 || got[0] != testHook.Command {
		t.Errorf("Stop commands = %q", got)
	}
}

func TestMergeSettingsRejectsInvalid(t *testing.T) {
	for _, old := range []string{"{not json", `{"hooks": []}`} {
		if _, _, err := mergeSettings([]byte(old), testHook, false); err == nil {
			t.Errorf("accepted %s", old)
		}
	}
}

// TestMinimalPatchVerified: when the in-place patch would not produce the
// intended settings (here: duplicate "Stop" keys, where the patch edits the
// first but decoders keep the last), the result is re-marshaled instead.
func TestMinimalPatchVerified(t *testing.T) {
	old := `{"hooks": {"Stop": [], "Stop": []}}`
	out, note, err := mergeSettings([]byte(old), testHook, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "rewrote the whole file") {
		t.Errorf("note = %q, want the rewrite fallback", note)
	}
	if got := stopCommands(t, out); len(got) != 1 || got[0] != testHook.Command {
		t.Errorf("Stop commands after fallback = %q", got)
	}
}

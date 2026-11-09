package adopt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/extremenelson/doneness/internal/gate"
)

// hookEntry is the Stop-hook object adopt installs.
type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

func (h hookEntry) compact() string {
	cmd, _ := gate.EncodeJSON(h.Command, false)
	return fmt.Sprintf(`{"type": "command", "command": %s, "timeout": %d}`, cmd, h.Timeout)
}

// mergeSettings returns settings content with h as the gate's Stop hook:
// an existing gate hook (any command gate.IsGateHook accepts) is replaced,
// otherwise a new Stop group is added. Other settings are kept.
//
// minimal selects a text patch that leaves the rest of the file byte for
// byte (for committed files, D17); it falls back to re-marshaling (and says
// so in note) when the file has no "hooks" object or the patch would not
// parse. old == nil means the file does not exist yet.
func mergeSettings(old []byte, h hookEntry, minimal bool) (out []byte, note string, err error) {
	if len(bytes.TrimSpace(old)) == 0 {
		return marshalSettings(map[string]any{}, h)
	}
	var doc map[string]any
	if err := json.Unmarshal(old, &doc); err != nil {
		return nil, "", fmt.Errorf("existing settings are not valid JSON (%v); fix or remove the file and re-run", err)
	}
	if !minimal {
		return marshalSettings(doc, h)
	}
	if patched, ok := patchSettings(old, h); ok {
		return patched, "", nil
	}
	out, _, err = marshalSettings(doc, h)
	return out, "rewrote the whole file: no \"hooks\" object to patch in place", err
}

// marshalSettings inserts h into a decoded settings document and
// re-encodes it with two-space indentation.
func marshalSettings(doc map[string]any, h hookEntry) ([]byte, string, error) {
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		if _, present := doc["hooks"]; present {
			return nil, "", errors.New(`settings "hooks" is not an object`)
		}
		hooks = map[string]any{}
	}
	stop, _ := hooks["Stop"].([]any)
	entry := map[string]any{"type": "command", "command": h.Command, "timeout": h.Timeout}
	replaced := false
	for _, g := range stop {
		group, _ := g.(map[string]any)
		list, _ := group["hooks"].([]any)
		for i, item := range list {
			m, _ := item.(map[string]any)
			if c, _ := m["command"].(string); gate.IsGateHook(c) {
				list[i] = entry
				replaced = true
			}
		}
	}
	if !replaced {
		stop = append(stop, map[string]any{"hooks": []any{entry}})
	}
	hooks["Stop"] = stop
	doc["hooks"] = hooks
	b, err := gate.EncodeJSON(doc, true)
	if err != nil {
		return nil, "", err
	}
	return b, "", nil
}

// node is a JSON value with its byte span in the source.
type node struct {
	start, end int // [start, end)
	delim      byte
	keys       []string // object member names, in order
	kids       []*node  // object member values or array elements
	str        string   // string scalar value
}

func (n *node) get(key string) *node {
	for i, k := range n.keys {
		if k == key {
			return n.kids[i]
		}
	}
	return nil
}

// parseSpans decodes b into a node tree carrying byte offsets.
func parseSpans(b []byte) (*node, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var value func() (*node, error)
	value = func() (*node, error) {
		start := int(dec.InputOffset())
		for start < len(b) && strings.IndexByte(" \t\r\n:,", b[start]) >= 0 {
			start++
		}
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		n := &node{start: start}
		switch t := tok.(type) {
		case json.Delim:
			n.delim = byte(t)
			for dec.More() {
				if t == '{' {
					k, err := dec.Token()
					if err != nil {
						return nil, err
					}
					n.keys = append(n.keys, k.(string))
				}
				kid, err := value()
				if err != nil {
					return nil, err
				}
				n.kids = append(n.kids, kid)
			}
			if _, err := dec.Token(); err != nil { // closing delimiter
				return nil, err
			}
		case string:
			n.str = t
		}
		n.end = int(dec.InputOffset())
		return n, nil
	}
	return value()
}

// patchSettings splices h into b without reformatting anything else.
func patchSettings(b []byte, h hookEntry) ([]byte, bool) {
	root, err := parseSpans(b)
	if err != nil || root.delim != '{' {
		return nil, false
	}
	hooks := root.get("hooks")
	if hooks == nil || hooks.delim != '{' {
		return nil, false
	}
	group := `{"hooks": [` + h.compact() + `]}`

	var out []byte
	switch stop := hooks.get("Stop"); {
	case stop == nil:
		member := `"Stop": [` + group + `]`
		out = insertMember(b, hooks, member)
	case stop.delim != '[':
		return nil, false
	default:
		if existing := findGateHook(stop); existing != nil {
			out = splice(b, existing.start, existing.end, h.compact())
		} else {
			out = insertMember(b, stop, group)
		}
	}

	// The patch must parse and contain exactly the hook we meant to add.
	var check struct {
		Hooks struct {
			Stop []struct {
				Hooks []hookEntry `json:"hooks"`
			} `json:"Stop"`
		} `json:"hooks"`
	}
	if json.Unmarshal(out, &check) != nil {
		return nil, false
	}
	for _, g := range check.Hooks.Stop {
		for _, e := range g.Hooks {
			if e == h {
				return out, true
			}
		}
	}
	return nil, false
}

// findGateHook returns the hook object inside a Stop array whose command
// runs the gate.
func findGateHook(stop *node) *node {
	for _, g := range stop.kids {
		list := g.get("hooks")
		if g.delim != '{' || list == nil || list.delim != '[' {
			continue
		}
		for _, item := range list.kids {
			if item.delim != '{' {
				continue
			}
			if c := item.get("command"); c != nil && gate.IsGateHook(c.str) {
				return item
			}
		}
	}
	return nil
}

// insertMember adds text as the last member/element of container, on its
// own line, indented like its siblings.
func insertMember(b []byte, container *node, text string) []byte {
	if n := len(container.kids); n > 0 {
		last := container.kids[n-1]
		// An object member's value starts on its key's line, so the value's
		// line indent is the member indent.
		indent := lineIndent(b, last.start)
		return splice(b, last.end, last.end, ",\n"+indent+text)
	}
	outer := lineIndent(b, container.start)
	return splice(b, container.start+1, container.end-1, "\n"+outer+indentUnit(b)+text+"\n"+outer)
}

func lineIndent(b []byte, pos int) string {
	start := bytes.LastIndexByte(b[:pos], '\n') + 1
	end := start
	for end < len(b) && (b[end] == ' ' || b[end] == '\t') {
		end++
	}
	return string(b[start:end])
}

// indentUnit guesses the file's indentation step: a tab, or the smallest
// run of leading spaces, defaulting to two spaces.
func indentUnit(b []byte) string {
	best := ""
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		lead := line[:len(line)-len(trimmed)]
		if lead == "" || trimmed == "" {
			continue
		}
		if strings.HasPrefix(lead, "\t") {
			return "\t"
		}
		if best == "" || len(lead) < len(best) {
			best = lead
		}
	}
	if best == "" {
		return "  "
	}
	return best
}

func splice(b []byte, from, to int, text string) []byte {
	out := make([]byte, 0, len(b)+len(text))
	out = append(out, b[:from]...)
	out = append(out, text...)
	return append(out, b[to:]...)
}

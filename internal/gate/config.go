// Package gate owns the enforced definition of done: the per-repo gate
// config, the Claude Code Stop-hook command that runs the gate, and (in later
// changes) running the check itself. Other packages (adopt, scaffold) read and
// write these through this package so there is exactly one definition of each.
package gate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ConfigFile is the gate config's name, at the repository's top level.
const ConfigFile = ".newproj.json"

var (
	// ErrNoConfig means the repository has no ConfigFile.
	ErrNoConfig = errors.New("no " + ConfigFile + " (run `newproj adopt`)")
	// ErrEmptyCommand means the config names no check command.
	ErrEmptyCommand = errors.New(ConfigFile + ": gate.command is empty")
)

// Config is the contents of ConfigFile: {"gate":{"command":"make check"}}.
type Config struct {
	Gate Gate `json:"gate"`
}

// Gate is the gate section of the config.
type Gate struct {
	// Command is run with `sh -c` at the repository's top level. It must
	// only verify: a command that writes tracked files fails the gate.
	Command string `json:"command"`
}

// Validate reports whether c can be used to run a gate.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Gate.Command) == "" {
		return ErrEmptyCommand
	}
	return nil
}

// Hash identifies the gate settings a receipt was produced under: the
// sha256 of the canonical JSON of the gate section, so reformatting the
// file does not change it but changing the command does.
func (c Config) Hash() string {
	b, err := json.Marshal(c.Gate)
	if err != nil {
		// Gate holds only strings; Marshal cannot fail on it.
		panic(fmt.Sprintf("gate: marshal config: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// LoadConfig reads and validates ConfigFile in the repository rooted at root.
// Unknown keys are errors, so a misspelled key cannot silently disable a
// setting.
func LoadConfig(root string) (Config, error) {
	path := filepath.Join(root, ConfigFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, ErrNoConfig
	}
	if err != nil {
		return Config{}, err
	}

	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%s: unexpected data after the config object", ConfigFile)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// WriteConfig validates c and writes it to ConfigFile under root, replacing
// any existing file atomically.
func WriteConfig(root string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(root, ConfigFile+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(root, ConfigFile))
}

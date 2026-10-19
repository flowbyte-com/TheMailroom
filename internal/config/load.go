package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Load reads, parses and validates a configuration file at path.
//
// On success the returned Config is normalised (defaults applied) and ready
// to be turned into a server. On any failure a typed error is returned
// together with the path that caused it.
func Load(path string) (Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: resolve path: %w", err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", abs, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", abs, err)
	}
	return cfg, nil
}

// LoadFromFile loads a configuration file from path with no transformation.
func LoadFromFile(path string) (Config, error) { return Load(path) }

// Parse parses and validates raw YAML bytes.
//
// The parser is strict: unknown fields are rejected, and a separate
// duplicate-key scan prevents silent overrides by a later map entry.
func Parse(data []byte) (Config, error) {
	if err := checkDuplicateKeys(data); err != nil {
		return Config{}, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse yaml: %w", err)
	}
	// Allow empty trailing document.
	if err := dec.Decode(new(struct{})); err != nil && err.Error() != "EOF" {
		return Config{}, fmt.Errorf("parse yaml (trailing): %w", err)
	}
	return cfg.Validate()
}

// checkDuplicateKeys decodes the YAML into a generic map first and
// walks it, rejecting any object that contains the same key twice.
// yaml.v3 itself silently overwrites duplicate keys, which is a
// footgun for configuration files: a typo followed by the correct
// spelling would otherwise pass unnoticed.
func checkDuplicateKeys(data []byte) error {
	var probe yaml.Node
	if err := yaml.Unmarshal(data, &probe); err != nil {
		// We re-run the real decode below for a typed error, so just
		// report a parse error here and let Parse() surface details.
		return fmt.Errorf("parse yaml: %w", err)
	}
	return walkDuplicateKeys(&probe)
}

func walkDuplicateKeys(n *yaml.Node) error {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			if err := walkDuplicateKeys(c); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		seen := make(map[string]int, len(n.Content)/2)
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			val := n.Content[i+1]
			if key.Value == "" {
				continue
			}
			if line, ok := seen[key.Value]; ok {
				return fmt.Errorf("duplicate key %q at line %d (first occurrence at line %d)", key.Value, key.Line, line)
			}
			seen[key.Value] = key.Line
			if err := walkDuplicateKeys(val); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if err := walkDuplicateKeys(c); err != nil {
				return err
			}
		}
	}
	return nil
}

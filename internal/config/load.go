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

// Parse parses and validates raw YAML bytes.
func Parse(data []byte) (Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(false) // tolerate unknown keys but log them in future
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

// LoadFromFile loads a configuration file from path with no transformation.
func LoadFromFile(path string) (Config, error) { return Load(path) }

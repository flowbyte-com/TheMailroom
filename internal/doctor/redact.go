package doctor

import (
	"errors"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// linePattern extracts the line reference from a YAML error. A line number
// is a safe fact to report.
var linePattern = regexp.MustCompile(`lines? ([0-9]+(?:-[0-9]+)?)`)

// validationField matches the trusted field path that begins a loader
// validation message, for example "accounts[0].permissions" or
// "server.max_request_bytes".
//
// The path is assembled by the loader from literal field names and an
// account index, so it is a trusted identifier. The rest of the message can
// embed a configured value, in any form, so it is not printed.
var validationField = regexp.MustCompile(
	`^[a-z][a-z0-9_]*(?:\[[0-9]+\])?(?:\.[a-z][a-z0-9_]*(?:\[[0-9]+\])?)+`)

// describeConfigError renders a configuration failure without echoing any
// configured value.
//
// The document and the loader's explanation are both treated as untrusted.
// A YAML syntax, type, unknown-field or duplicate-key error quotes or names
// document content, so only a fixed description and a line number are
// printed. A validation error names a trusted field path and then explains
// the failure with text that can embed a configured value; only the field
// path is printed.
//
// This is fail-closed by construction: no part of a configured value is
// ever copied into the report, so a value cannot be smuggled through a
// quoted run, a path, an alias or a longer word that contains it.
func describeConfigError(err error, data []byte) string {
	if err == nil {
		return ""
	}

	// A syntax error means the document cannot be trusted at all.
	var probe yaml.Node
	if yamlErr := yaml.Unmarshal(data, &probe); yamlErr != nil {
		return "the configuration is not valid YAML" + lineSuffix(yamlErr.Error()) +
			"; the file content is not printed"
	}

	// A type or unknown-field error quotes the offending scalar or names a
	// field. Neither is printed.
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		return "the configuration does not match the schema" + lineSuffix(typeErr.Error()) +
			"; the offending value is not printed"
	}

	// The duplicate-key scan reports the key it found. A YAML key is
	// document content, so it is not printed.
	if strings.Contains(err.Error(), "duplicate key") {
		return "the configuration contains a duplicate key" +
			"; the key is not printed"
	}

	// A validation error: keep the trusted field path, drop the
	// explanation, which can embed a value.
	if field := validationField.FindString(err.Error()); field != "" {
		return "the configuration is invalid at " + field +
			"; the rejected value is not printed"
	}
	return "the configuration failed validation; the rejected value is not printed"
}

// lineSuffix renders a YAML error's line reference, if it has one.
func lineSuffix(msg string) string {
	m := linePattern.FindStringSubmatch(msg)
	if m == nil {
		return ""
	}
	return " (line " + m[1] + ")"
}

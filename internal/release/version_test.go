package release

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// versionSourceFile is the source of the package that holds the
// build-time variables, relative to the repository root.
const versionSourceFile = "internal/version/version.go"

// TestVersionVariablePathMatchesTheModule pins that the linker flags
// name the package they are meant to name.
//
// The -X flags address a variable by its fully qualified name. If the
// module path changes and the flag does not, the linker silently ignores
// the flag and the binary reports the development defaults.
func TestVersionVariablePathMatchesTheModule(t *testing.T) {
	root := repoRoot(t)

	data, err := os.ReadFile(filepath.Join(root, moduleFile))
	if err != nil {
		t.Fatalf("read %s: %v", moduleFile, err)
	}
	modulePath := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			modulePath = strings.TrimSpace(strings.TrimPrefix(line, "module "))
			break
		}
	}
	if modulePath == "" {
		t.Fatalf("%s declares no module path", moduleFile)
	}

	want := modulePath + "/internal/version"
	if versionVarPath != want {
		t.Errorf("the linker flags name %q, want %q", versionVarPath, want)
	}
}

// TestVersionPackageDeclaresTheLinkedVariables pins that the variables
// the linker flags set are declared, as package-level string variables.
//
// The linker ignores a flag that names a symbol which does not exist, so
// a rename of one of these variables would silently produce a binary
// that reports the development defaults. The variables must also be
// variables and not constants: the linker can only set a variable.
func TestVersionPackageDeclaresTheLinkedVariables(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, versionSourceFile)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", versionSourceFile, err)
	}

	declared := make(map[string]string)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range value.Names {
				declared[name.Name] = declaredType(value)
			}
		}
	}

	for _, name := range []string{"Version", "Commit", "BuildDate"} {
		typeName, ok := declared[name]
		if !ok {
			t.Errorf("%s does not declare the variable %q", versionSourceFile, name)
			continue
		}
		if typeName != "string" {
			t.Errorf("%s declares %q as %q, want a string", versionSourceFile, name, typeName)
		}
	}
}

// declaredType reports the type of a variable declaration.
//
// The type may be written out or inferred from the initial value. The
// linker can only set a string variable, so an inferred type is accepted
// only when it is inferred from a string literal.
func declaredType(value *ast.ValueSpec) string {
	if value.Type != nil {
		if ident, ok := value.Type.(*ast.Ident); ok {
			return ident.Name
		}
		return ""
	}
	if len(value.Values) == 1 {
		if lit, ok := value.Values[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			return "string"
		}
	}
	return ""
}

// TestLDFlagsSetExactlyTheThreeVariables pins the set of linker flags.
//
// Each of the three metadata values must be set by exactly one flag, and
// no flag may address a variable that does not exist.
func TestLDFlagsSetExactlyTheThreeVariables(t *testing.T) {
	meta := Metadata{
		Version:   "1.2.3",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		BuildDate: "2026-11-02T12:00:00Z",
	}
	flags := LDFlags(meta)

	// The linker takes the value as the argument that follows the flag,
	// so "-X path=value" is two fields.
	fields := strings.Fields(flags)
	var set []string
	for i := 0; i < len(fields); i++ {
		if fields[i] != "-X" {
			continue
		}
		if i+1 >= len(fields) {
			t.Fatalf("the linker flag %q has no value", fields[i])
		}
		i++
		target, _, found := strings.Cut(fields[i], "=")
		if !found {
			t.Errorf("the linker flag %q has no value", fields[i])
			continue
		}
		set = append(set, target)
	}

	want := []string{
		versionVarPath + ".Version",
		versionVarPath + ".Commit",
		versionVarPath + ".BuildDate",
	}
	if len(set) != len(want) {
		t.Fatalf("the linker flags set %d variables, want %d: %v", len(set), len(want), set)
	}
	for _, target := range want {
		var found bool
		for _, got := range set {
			if got == target {
				found = true
			}
		}
		if !found {
			t.Errorf("the linker flags do not set %q: %v", target, set)
		}
	}
}

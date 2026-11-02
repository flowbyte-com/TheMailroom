package release

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestInspectArtifactBytesRejectsUnknownFormat reports a file that is not
// an executable, rather than guessing.
func TestInspectArtifactBytesRejectsUnknownFormat(t *testing.T) {
	if _, err := InspectArtifactBytes([]byte("not an executable")); err == nil {
		t.Error("InspectArtifactBytes() with plain text = nil, want an error")
	}
}

// TestInspectELF pins the ELF reader against a hand-built header.
//
// The reader is what decides whether a Linux artifact is statically
// linked, so it has to read the machine and the program headers
// correctly.
func TestInspectELF(t *testing.T) {
	// 64 bytes of ELF header plus one 56-byte program header.
	data := make([]byte, 64+56)
	copy(data, "\x7fELF")
	data[4] = 2                                    // 64-bit
	data[5] = 1                                    // little endian
	binary.LittleEndian.PutUint16(data[18:], 0x3E) // x86-64
	binary.LittleEndian.PutUint64(data[32:], 64)   // e_phoff
	binary.LittleEndian.PutUint16(data[54:], 56)   // e_phentsize
	binary.LittleEndian.PutUint16(data[56:], 1)    // e_phnum
	ph := data[64:]
	binary.LittleEndian.PutUint32(ph[0:], 1) // PT_LOAD

	got, err := InspectArtifactBytes(data)
	if err != nil {
		t.Fatalf("InspectArtifactBytes: %v", err)
	}
	if got.Format != FormatELF {
		t.Errorf("Format = %q, want %q", got.Format, FormatELF)
	}
	if got.Machine != "amd64" {
		t.Errorf("Machine = %q, want %q", got.Machine, "amd64")
	}
	if got.Bits != 64 {
		t.Errorf("Bits = %d, want 64", got.Bits)
	}
	if !got.Static {
		t.Error("Static = false, want true for a binary with no PT_INTERP")
	}
}

// TestInspectELFDetectsDynamicLinking covers the case the static check
// exists for: a binary with a PT_INTERP segment is dynamically linked.
func TestInspectELFDetectsDynamicLinking(t *testing.T) {
	data := make([]byte, 64+56)
	copy(data, "\x7fELF")
	data[4] = 2
	data[5] = 1
	binary.LittleEndian.PutUint16(data[18:], 0xB7) // AArch64
	binary.LittleEndian.PutUint64(data[32:], 64)
	binary.LittleEndian.PutUint16(data[54:], 56)
	binary.LittleEndian.PutUint16(data[56:], 1)
	ph := data[64:]
	binary.LittleEndian.PutUint32(ph[0:], 3) // PT_INTERP

	got, err := InspectArtifactBytes(data)
	if err != nil {
		t.Fatalf("InspectArtifactBytes: %v", err)
	}
	if got.Machine != "arm64" {
		t.Errorf("Machine = %q, want %q", got.Machine, "arm64")
	}
	if got.Static {
		t.Error("Static = true, want false for a binary with a PT_INTERP segment")
	}
}

// TestInspectMachO pins the Mach-O reader against a hand-built header.
func TestInspectMachO(t *testing.T) {
	data := make([]byte, 32)
	binary.LittleEndian.PutUint32(data[0:], 0xFEEDFACF) // 64-bit
	binary.LittleEndian.PutUint32(data[4:], 0x0100000C) // arm64
	binary.LittleEndian.PutUint32(data[16:], 0)         // ncmds

	got, err := InspectArtifactBytes(data)
	if err != nil {
		t.Fatalf("InspectArtifactBytes: %v", err)
	}
	if got.Format != FormatMachO {
		t.Errorf("Format = %q, want %q", got.Format, FormatMachO)
	}
	if got.Machine != "arm64" {
		t.Errorf("Machine = %q, want %q", got.Machine, "arm64")
	}
	if !got.Static {
		t.Error("Static = false, want true for a binary with no dylib load command")
	}
}

// TestInspectMachODetectsDylibDependency covers the dylib load command.
func TestInspectMachODetectsDylibDependency(t *testing.T) {
	data := make([]byte, 32+24)
	binary.LittleEndian.PutUint32(data[0:], 0xFEEDFACF)
	binary.LittleEndian.PutUint32(data[4:], 0x01000007) // x86-64
	binary.LittleEndian.PutUint32(data[16:], 1)         // ncmds
	cmd := data[32:]
	binary.LittleEndian.PutUint32(cmd[0:], 0x0C) // LC_LOAD_DYLIB
	binary.LittleEndian.PutUint32(cmd[4:], 24)   // cmdsize

	got, err := InspectArtifactBytes(data)
	if err != nil {
		t.Fatalf("InspectArtifactBytes: %v", err)
	}
	if got.Machine != "amd64" {
		t.Errorf("Machine = %q, want %q", got.Machine, "amd64")
	}
	if got.Static {
		t.Error("Static = true, want false for a binary with a dylib load command")
	}
}

// TestInspectPE pins the PE reader against a hand-built header.
func TestInspectPE(t *testing.T) {
	// 0x40 bytes of DOS header, the PE signature, the file header and the
	// start of the optional header.
	data := make([]byte, 0x40+24+2)
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[0x3C:], 0x40)
	copy(data[0x40:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(data[0x44:], 0x8664) // x86-64
	binary.LittleEndian.PutUint16(data[0x58:], 0x20B)  // PE32+

	got, err := InspectArtifactBytes(data)
	if err != nil {
		t.Fatalf("InspectArtifactBytes: %v", err)
	}
	if got.Format != FormatPE {
		t.Errorf("Format = %q, want %q", got.Format, FormatPE)
	}
	if got.Machine != "amd64" {
		t.Errorf("Machine = %q, want %q", got.Machine, "amd64")
	}
	if got.Bits != 64 {
		t.Errorf("Bits = %d, want 64", got.Bits)
	}
}

// TestInspectArtifactMissingFile reports a missing artifact.
func TestInspectArtifactMissingFile(t *testing.T) {
	if _, err := InspectArtifact(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("InspectArtifact() with a missing file = nil, want an error")
	}
}

// TestInspectBuiltArtifact reads the header of a binary this repository
// actually builds.
//
// It is the check that the readers agree with the toolchain: a Go binary
// for this host must be recognised as the expected format and machine.
func TestInspectBuiltArtifact(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build in short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not available")
	}

	root := repoRoot(t)
	meta, err := ResolveMetadata(root)
	if err != nil {
		t.Fatalf("ResolveMetadata: %v", err)
	}
	dist := t.TempDir()
	target := Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	err = Build(BuildOptions{
		Root:     root,
		Dist:     dist,
		Metadata: meta,
		Targets:  []Target{target},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	got, err := InspectArtifact(filepath.Join(dist, ArtifactName(target)))
	if err != nil {
		t.Fatalf("InspectArtifact: %v", err)
	}
	if got.Format != formatFor(target) {
		t.Errorf("Format = %q, want %q", got.Format, formatFor(target))
	}
	if got.Machine != target.GOARCH {
		t.Errorf("Machine = %q, want %q", got.Machine, target.GOARCH)
	}
	if target.GOOS == "linux" && !got.Static {
		t.Error("Static = false, want true: the Linux artifact must be statically linked")
	}
}

// TestInspectBuiltArtifactIsStaticOnLinux pins the CGO-disabled
// requirement against the artifact this repository builds.
//
// A Linux release artifact that is not statically linked depends on the
// shared libraries of the machine it runs on, which is exactly what a
// CGO-disabled build exists to prevent.
func TestInspectBuiltArtifactIsStaticOnLinux(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build in short mode")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the static linkage check applies to Linux artifacts")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not available")
	}

	root := repoRoot(t)
	meta, err := ResolveMetadata(root)
	if err != nil {
		t.Fatalf("ResolveMetadata: %v", err)
	}
	dist := t.TempDir()
	err = Build(BuildOptions{
		Root:     root,
		Dist:     dist,
		Metadata: meta,
		Targets:  []Target{{GOOS: "linux", GOARCH: runtime.GOARCH}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	info, err := InspectArtifact(filepath.Join(dist, "themailroom-linux-"+runtime.GOARCH))
	if err != nil {
		t.Fatalf("InspectArtifact: %v", err)
	}
	if !info.Static {
		t.Error("the Linux artifact is not statically linked; CGO_ENABLED=0 must produce a static binary")
	}
}

// TestVerifyCommitEmbeddedRejectsWrongCommit covers the check that
// catches a linker flag which silently failed.
//
// The commit is the value that is scanned for, because forty
// hexadecimal characters cannot occur in a binary by accident. The
// version and the build date are checked by running the binary, which is
// conclusive; see verifyRuns.
func TestVerifyCommitEmbeddedRejectsWrongCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact")
	want := Metadata{Version: "1.0.0", Commit: "abc123", BuildDate: "2026-11-02T12:00:00Z"}
	if err := os.WriteFile(path, []byte(want.Version+" "+want.Commit+" "+want.BuildDate), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCommitEmbedded(path, want); err != nil {
		t.Fatalf("VerifyCommitEmbedded: %v", err)
	}
	wrong := Metadata{Version: "1.0.0", Commit: "def456", BuildDate: "2026-11-02T12:00:00Z"}
	if err := VerifyCommitEmbedded(path, wrong); err == nil {
		t.Error("VerifyCommitEmbedded() with the wrong commit = nil, want an error")
	}
}

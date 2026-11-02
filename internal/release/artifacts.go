package release

import (
	"encoding/binary"
	"fmt"
	"os"
)

// ArtifactFormat is the executable container format of an artifact.
type ArtifactFormat string

const (
	// FormatELF is the Linux executable format.
	FormatELF ArtifactFormat = "elf"
	// FormatMachO is the macOS executable format.
	FormatMachO ArtifactFormat = "mach-o"
	// FormatPE is the Windows executable format.
	FormatPE ArtifactFormat = "pe"
)

// ArtifactInfo describes the container of a release artifact.
type ArtifactInfo struct {
	// Format is the container format.
	Format ArtifactFormat
	// Machine is the CPU the artifact targets, in Go's GOARCH naming.
	Machine string
	// Bits is the width of the target in bits.
	Bits int
	// Static reports whether the artifact carries no dynamic loader and
	// no dynamic library dependency.
	//
	// It is meaningful for ELF, where a CGO-disabled binary is fully
	// static and depends on nothing but the kernel. A Mach-O binary
	// always links the system library and a Windows binary always
	// imports system DLLs, so the field is false for those platforms
	// and is not a defect there.
	Static bool
}

// InspectArtifact reads the header of the artifact at path and reports its
// container format, the machine it targets and whether it is statically
// linked.
//
// The check is done by reading the executable's own headers rather than
// by shelling out to file(1), so the result is exact and does not depend
// on the wording of another tool's output.
func InspectArtifact(path string) (ArtifactInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ArtifactInfo{}, err
	}
	return InspectArtifactBytes(data)
}

// InspectArtifactBytes reports the container of an in-memory executable.
func InspectArtifactBytes(data []byte) (ArtifactInfo, error) {
	switch {
	case len(data) >= 4 && string(data[:4]) == "\x7fELF":
		return inspectELF(data)
	case len(data) >= 4 && isMachOMagic(data[:4]):
		return inspectMachO(data)
	case len(data) >= 2 && string(data[:2]) == "MZ":
		return inspectPE(data)
	default:
		return ArtifactInfo{}, fmt.Errorf("release: %q is not a recognised executable format", "artifact")
	}
}

// inspectELF reads an ELF header and its program headers.
//
// The machine is read from e_machine and the dynamic loader from the
// presence of a PT_INTERP program header. A CGO-disabled Go binary has
// neither an interpreter nor a dynamic section, so it runs on a system
// with no matching shared libraries.
func inspectELF(data []byte) (ArtifactInfo, error) {
	if len(data) < 64 {
		return ArtifactInfo{}, fmt.Errorf("release: ELF header is truncated")
	}
	class := data[4]
	encoding := data[5]
	var bo binary.ByteOrder
	switch encoding {
	case 1:
		bo = binary.LittleEndian
	case 2:
		bo = binary.BigEndian
	default:
		return ArtifactInfo{}, fmt.Errorf("release: ELF byte order %d is not known", encoding)
	}

	var bits int
	var phoff, phentsize, phnum uint64
	switch class {
	case 1:
		bits = 32
		phoff = uint64(bo.Uint32(data[28:]))
		phentsize = uint64(bo.Uint16(data[42:]))
		phnum = uint64(bo.Uint16(data[44:]))
	case 2:
		bits = 64
		phoff = bo.Uint64(data[32:])
		phentsize = uint64(bo.Uint16(data[54:]))
		phnum = uint64(bo.Uint16(data[56:]))
	default:
		return ArtifactInfo{}, fmt.Errorf("release: ELF class %d is not known", class)
	}

	machine := elfMachine(bo.Uint16(data[18:]))
	info := ArtifactInfo{Format: FormatELF, Machine: machine, Bits: bits, Static: true}

	// A PT_INTERP segment means the kernel loads a dynamic linker, and a
	// PT_DYNAMIC segment means the binary resolves shared libraries.
	const (
		ptInterp  = 3
		ptDynamic = 2
	)
	// The program headers are read defensively: the file may be
	// truncated, malformed or hostile, and a parser that panics on such
	// a file turns a bad artifact into a crash.
	if phentsize < 4 {
		return info, nil
	}
	limit := uint64(len(data))
	if phoff > limit {
		return info, nil
	}
	for i := uint64(0); i < phnum; i++ {
		// The offset cannot overflow: phoff is bounded by the file
		// length and the product is bounded by the 16-bit sizes of
		// e_phnum and e_phentsize.
		off := phoff + i*phentsize
		if off+4 > limit {
			break
		}
		ptype := bo.Uint32(data[off : off+4])
		switch ptype {
		case ptInterp, ptDynamic:
			info.Static = false
		}
	}
	return info, nil
}

// elfMachine maps an ELF e_machine value to a GOARCH name.
func elfMachine(m uint16) string {
	switch m {
	case 0x03:
		return "386"
	case 0x28:
		return "arm"
	case 0x3E:
		return "amd64"
	case 0xB7:
		return "arm64"
	case 0xF3:
		return "riscv64"
	default:
		return fmt.Sprintf("unknown(%d)", m)
	}
}

// isMachOMagic reports whether the four bytes are a Mach-O magic number.
//
// The 64-bit little-endian magic is 0xFEEDFACF, which appears in the
// file as the bytes CF FA ED FE.
func isMachOMagic(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	return (b[0] == 0xCF && b[1] == 0xFA && b[2] == 0xED && b[3] == 0xFE) ||
		(b[0] == 0xCE && b[1] == 0xFA && b[2] == 0xED && b[3] == 0xFE)
}

// inspectMachO reads a Mach-O header and walks its load commands.
//
// The CPU type is read from cputype. The load commands are walked to
// find a dylib load command, whose presence means the binary depends on
// a shared library outside the executable.
func inspectMachO(data []byte) (ArtifactInfo, error) {
	if len(data) < 32 {
		return ArtifactInfo{}, fmt.Errorf("release: Mach-O header is truncated")
	}
	bo := binary.LittleEndian
	magic := bo.Uint32(data[0:4])
	bits := 64
	if magic == 0xFEEDFACE {
		bits = 32
	}
	cputype := bo.Uint32(data[4:8])
	ncmds := bo.Uint32(data[16:20])

	info := ArtifactInfo{
		Format:  FormatMachO,
		Machine: machOMachine(cputype),
		Bits:    bits,
		Static:  true,
	}

	const (
		lcLoadDylib       = 0x0C
		lcIDDylib         = 0x0D
		lcLoadWeakDylib   = 0x18
		lcReexportDylib   = 0x1F
		lcLazyLoadDylib   = 0x20
		lcLoadUpwardDylib = 0x23
	)
	headerSize := uint32(28)
	if bits == 64 {
		headerSize = 32
	}
	// The walk uses 64-bit offsets and is bounded against the file
	// length, so a malformed load command cannot wrap the offset back
	// into the buffer and make the walk continue over garbage.
	limit := uint64(len(data))
	off := uint64(headerSize)
	for i := uint32(0); i < ncmds; i++ {
		if off+8 > limit {
			break
		}
		cmd := bo.Uint32(data[off : off+4])
		cmdsize := uint64(bo.Uint32(data[off+4 : off+8]))
		if cmdsize < 8 {
			break
		}
		switch cmd {
		case lcLoadDylib, lcIDDylib, lcLoadWeakDylib, lcReexportDylib, lcLazyLoadDylib, lcLoadUpwardDylib:
			info.Static = false
		}
		if cmdsize > limit-off {
			break
		}
		off += cmdsize
	}
	return info, nil
}

// machOMachine maps a Mach-O CPU type to a GOARCH name.
func machOMachine(cputype uint32) string {
	switch cputype {
	case 7:
		return "386"
	case 0x01000007:
		return "amd64"
	case 12:
		return "arm"
	case 0x0100000C:
		return "arm64"
	default:
		return fmt.Sprintf("unknown(%d)", cputype)
	}
}

// inspectPE reads a PE header.
//
// The machine is read from the IMAGE_FILE_HEADER. The import table is
// located but not required to be empty: a CGO-disabled Windows binary
// still imports a handful of system DLLs, so Windows artifacts are
// verified by format and machine rather than by static linkage.
func inspectPE(data []byte) (ArtifactInfo, error) {
	if len(data) < 0x40 {
		return ArtifactInfo{}, fmt.Errorf("release: PE header is truncated")
	}
	bo := binary.LittleEndian
	peoff := bo.Uint32(data[0x3C:0x40])
	if uint64(peoff)+24 > uint64(len(data)) {
		return ArtifactInfo{}, fmt.Errorf("release: PE header offset %d is out of range", peoff)
	}
	if string(data[peoff:peoff+4]) != "PE\x00\x00" {
		return ArtifactInfo{}, fmt.Errorf("release: PE signature is missing")
	}
	machine := peMachine(bo.Uint16(data[peoff+4 : peoff+6]))
	bits := 32
	optional := peoff + 24
	if uint64(optional)+2 <= uint64(len(data)) {
		switch bo.Uint16(data[optional : optional+2]) {
		case 0x20B:
			bits = 64
		case 0x10B:
			bits = 32
		}
	}
	return ArtifactInfo{
		Format:  FormatPE,
		Machine: machine,
		Bits:    bits,
		Static:  false,
	}, nil
}

// peMachine maps a PE machine value to a GOARCH name.
func peMachine(m uint16) string {
	switch m {
	case 0x014C:
		return "386"
	case 0x01C0:
		return "arm"
	case 0x8664:
		return "amd64"
	case 0xAA64:
		return "arm64"
	default:
		return fmt.Sprintf("unknown(%d)", m)
	}
}

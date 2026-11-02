package release

import (
	"encoding/binary"
	"testing"
)

// The fixtures below build executable headers by hand, so that the
// readers can be tested against structures that are valid, and against
// structures that are not, without needing a toolchain or a set of
// real binaries of each platform.

// validELF64 builds a well-formed 64-bit ELF header with one program
// header of the given type.
//
// The layout follows the ELF64 specification: e_type at 16, e_machine
// at 18, e_version at 20, e_phoff at 32, e_ehsize at 52, e_phentsize at
// 54 and e_phnum at 56.
func validELF64(machine uint16, segmentType uint32) []byte {
	const (
		ehsize    = 64
		phentsize = 56
	)
	data := make([]byte, ehsize+phentsize)
	copy(data, "\x7fELF")
	data[4] = 2 // EI_CLASS: 64-bit
	data[5] = 1 // EI_DATA: little endian
	data[6] = 1 // EI_VERSION
	bo := binary.LittleEndian
	bo.PutUint16(data[16:], 2)         // e_type: ET_EXEC
	bo.PutUint16(data[18:], machine)   // e_machine
	bo.PutUint32(data[20:], 1)         // e_version: EV_CURRENT
	bo.PutUint64(data[32:], ehsize)    // e_phoff
	bo.PutUint16(data[52:], ehsize)    // e_ehsize
	bo.PutUint16(data[54:], phentsize) // e_phentsize
	bo.PutUint16(data[56:], 1)         // e_phnum
	bo.PutUint32(data[ehsize:], segmentType)
	return data
}

// validELF32 builds a well-formed 32-bit ELF header with one program
// header of the given type.
//
// The layout follows the ELF32 specification: e_type at 16, e_machine
// at 18, e_version at 20, e_phoff at 28, e_ehsize at 40, e_phentsize at
// 42 and e_phnum at 44.
func validELF32(machine uint16, segmentType uint32) []byte {
	const (
		ehsize    = 52
		phentsize = 32
	)
	data := make([]byte, ehsize+phentsize)
	copy(data, "\x7fELF")
	data[4] = 1 // EI_CLASS: 32-bit
	data[5] = 1 // EI_DATA: little endian
	data[6] = 1 // EI_VERSION
	bo := binary.LittleEndian
	bo.PutUint16(data[16:], 2)         // e_type: ET_EXEC
	bo.PutUint16(data[18:], machine)   // e_machine
	bo.PutUint32(data[20:], 1)         // e_version: EV_CURRENT
	bo.PutUint32(data[28:], ehsize)    // e_phoff
	bo.PutUint16(data[40:], ehsize)    // e_ehsize
	bo.PutUint16(data[42:], phentsize) // e_phentsize
	bo.PutUint16(data[44:], 1)         // e_phnum
	bo.PutUint32(data[ehsize:], segmentType)
	return data
}

// machOCommand is one Mach-O load command.
type machOCommand struct {
	// cmd is the load command identifier.
	cmd uint32
	// size is the size of the load command in bytes.
	size uint32
}

// validMachO64 builds a well-formed 64-bit Mach-O header with the given
// load commands.
//
// The layout follows the Mach-O 64-bit header: cputype at 4, filetype
// at 12, ncmds at 16 and sizeofcmds at 20. The load commands follow
// the 32-byte header.
//
// The buffer carries some slack beyond the load command region, so that
// a fixture with an inconsistent command size can still be written
// without panicking. The reader validates against sizeofcmds, so the
// slack does not make an inconsistent header look consistent.
func validMachO64(cputype uint32, commands []machOCommand) []byte {
	const headerSize = 32
	total := uint32(0)
	for _, c := range commands {
		total += c.size
	}
	data := make([]byte, headerSize+int(total)+64)
	bo := binary.LittleEndian
	bo.PutUint32(data[0:], 0xFEEDFACF) // MH_MAGIC_64
	bo.PutUint32(data[4:], cputype)
	bo.PutUint32(data[12:], 2) // MH_EXECUTE
	bo.PutUint32(data[16:], uint32(len(commands)))
	bo.PutUint32(data[20:], total)
	off := headerSize
	for _, c := range commands {
		bo.PutUint32(data[off:], c.cmd)
		bo.PutUint32(data[off+4:], c.size)
		off += int(c.size)
	}
	return data
}

// validPE builds a well-formed PE header.
//
// The layout follows the PE specification: e_lfanew at 0x3C, the file
// header at e_lfanew+4 with the machine at +4 and SizeOfOptionalHeader
// at +16, and the optional header at e_lfanew+24.
func validPE(machine uint16, optionalMagic uint16) []byte {
	const (
		peoff         = 0x40
		fileHeaderLen = 24
		optionalLen   = 240
	)
	data := make([]byte, peoff+fileHeaderLen+optionalLen)
	copy(data, "MZ")
	bo := binary.LittleEndian
	bo.PutUint32(data[0x3C:], peoff)
	copy(data[peoff:], "PE\x00\x00")
	bo.PutUint16(data[peoff+4:], machine)
	bo.PutUint16(data[peoff+20:], optionalLen) // SizeOfOptionalHeader
	bo.PutUint16(data[peoff+22:], 0x0022)      // characteristics: executable, large address aware
	bo.PutUint16(data[peoff+24:], optionalMagic)
	return data
}

// benignMachOCommands is a load command region that is valid but names
// no dylib, so a binary built from it is statically linked.
var benignMachOCommands = []machOCommand{{cmd: 0x19, size: 72}} // LC_SEGMENT_64

// TestInspectELFRejectsMalformedHeaders covers ELF headers that cannot
// be trusted.
//
// The verification must fail closed: a header that cannot be read
// cannot be used to conclude that an artifact is statically linked, so
// it has to be an error rather than a best guess.
func TestInspectELFRejectsMalformedHeaders(t *testing.T) {
	cases := map[string]func([]byte){
		"the header size does not match the class": func(data []byte) {
			binary.LittleEndian.PutUint16(data[52:], 32)
		},
		"the version is not the current one": func(data []byte) {
			binary.LittleEndian.PutUint32(data[20:], 2)
		},
		"the type is not an executable": func(data []byte) {
			binary.LittleEndian.PutUint16(data[16:], 1) // ET_REL
		},
		"the machine is not known": func(data []byte) {
			binary.LittleEndian.PutUint16(data[18:], 0x1234)
		},
		"there are no program headers": func(data []byte) {
			binary.LittleEndian.PutUint16(data[56:], 0)
		},
		"the program header size is too small": func(data []byte) {
			binary.LittleEndian.PutUint16(data[54:], 2)
		},
		"the program header offset is zero": func(data []byte) {
			binary.LittleEndian.PutUint64(data[32:], 0)
		},
		"the program header offset is out of range": func(data []byte) {
			binary.LittleEndian.PutUint64(data[32:], 0xFFFFFFFFFFFFFFF0)
		},
		"the program header table is truncated": func(data []byte) {
			binary.LittleEndian.PutUint16(data[56:], 100)
		},
		"the program header count is the maximum": func(data []byte) {
			binary.LittleEndian.PutUint16(data[56:], 0xFFFF)
		},
	}
	for name, mutate := range cases {
		data := validELF64(0x3E, 1)
		mutate(data)
		if _, err := InspectArtifactBytes(data); err == nil {
			t.Errorf("InspectArtifactBytes() with %s = nil, want an error", name)
		}
	}
}

// TestInspectMachORejectsMalformedHeaders covers Mach-O headers that
// cannot be trusted.
func TestInspectMachORejectsMalformedHeaders(t *testing.T) {
	cases := map[string]func([]byte){
		"the file type is not an executable": func(data []byte) {
			binary.LittleEndian.PutUint32(data[12:], 6) // MH_DYLIB
		},
		"the CPU type is not known": func(data []byte) {
			binary.LittleEndian.PutUint32(data[4:], 0x00FFFFFF)
		},
		"there are no load commands": func(data []byte) {
			binary.LittleEndian.PutUint32(data[16:], 0)
		},
		"the load command region is empty": func(data []byte) {
			binary.LittleEndian.PutUint32(data[20:], 0)
		},
		"the load command region is truncated": func(data []byte) {
			binary.LittleEndian.PutUint32(data[20:], 0xFFFFFF)
		},
		"the load command count does not match the region": func(data []byte) {
			binary.LittleEndian.PutUint32(data[16:], 5)
		},
	}
	for name, mutate := range cases {
		data := validMachO64(0x0100000C, benignMachOCommands)
		mutate(data)
		if _, err := InspectArtifactBytes(data); err == nil {
			t.Errorf("InspectArtifactBytes() with %s = nil, want an error", name)
		}
	}
}

// TestInspectMachORejectsMalformedLoadCommands covers load commands that
// are inconsistent with each other.
func TestInspectMachORejectsMalformedLoadCommands(t *testing.T) {
	cases := map[string][]machOCommand{
		"a command size that is too small":   {{cmd: 0x19, size: 4}},
		"a command size that is not aligned": {{cmd: 0x19, size: 25}},
		"a command size of zero":             {{cmd: 0x19, size: 0}},
	}
	for name, commands := range cases {
		data := validMachO64(0x0100000C, commands)
		if _, err := InspectArtifactBytes(data); err == nil {
			t.Errorf("InspectArtifactBytes() with %s = nil, want an error", name)
		}
	}
}

// TestInspectPERejectsMalformedHeaders covers PE headers that cannot be
// trusted.
func TestInspectPERejectsMalformedHeaders(t *testing.T) {
	cases := map[string]func([]byte){
		"the header offset is out of range": func(data []byte) {
			binary.LittleEndian.PutUint32(data[0x3C:], 0xFFFFFF)
		},
		"the signature is missing": func(data []byte) {
			copy(data[0x40:], "PX\x00\x00")
		},
		"the machine is not known": func(data []byte) {
			binary.LittleEndian.PutUint16(data[0x44:], 0x1234)
		},
		"there is no optional header": func(data []byte) {
			binary.LittleEndian.PutUint16(data[0x54:], 0)
		},
		"the optional header is truncated": func(data []byte) {
			binary.LittleEndian.PutUint16(data[0x54:], 0xFFFF)
		},
		"the optional header magic is not known": func(data []byte) {
			binary.LittleEndian.PutUint16(data[0x58:], 0x1234)
		},
		"the file is a dynamic library": func(data []byte) {
			binary.LittleEndian.PutUint16(data[0x56:], 0x2000)
		},
	}
	for name, mutate := range cases {
		data := validPE(0x8664, 0x20B)
		mutate(data)
		if _, err := InspectArtifactBytes(data); err == nil {
			t.Errorf("InspectArtifactBytes() with %s = nil, want an error", name)
		}
	}
}

// TestInspectPERejectsATinyOptionalHeader covers a PE file whose
// optional header is too small to hold its own magic.
//
// The magic is two bytes. A file that declares a one-byte optional
// header and ends exactly at that boundary satisfies the truncation
// check, so the parser has to refuse it on its own terms. Reading the
// magic without that check would read past the end of the file.
func TestInspectPERejectsATinyOptionalHeader(t *testing.T) {
	const peoff = 0x40
	// The file ends exactly at the declared end of the optional header.
	data := make([]byte, peoff+24+1)
	copy(data, "MZ")
	bo := binary.LittleEndian
	bo.PutUint32(data[0x3C:], peoff)
	copy(data[peoff:], "PE\x00\x00")
	bo.PutUint16(data[peoff+4:], 0x8664) // machine: x86-64
	bo.PutUint16(data[peoff+20:], 1)     // SizeOfOptionalHeader: one byte
	bo.PutUint16(data[peoff+22:], 0x0022)

	if _, err := InspectArtifactBytes(data); err == nil {
		t.Error("InspectArtifactBytes() with a one-byte optional header = nil, want an error")
	}
}

// TestInspectELFRejectsAnEntrySizeThatDoesNotMatchTheClass covers an
// ELF file whose program header entry size is inconsistent with its
// class.
//
// The entry size of a program header is fixed by the class: 56 bytes
// for a 64-bit file and 32 bytes for a 32-bit one. A 64-bit file that
// declares four-byte entries is inconsistent, and a parser that accepted
// it would be walking the program header table with the wrong stride.
func TestInspectELFRejectsAnEntrySizeThatDoesNotMatchTheClass(t *testing.T) {
	t.Run("64-bit", func(t *testing.T) {
		data := validELF64(0x3E, 1)
		binary.LittleEndian.PutUint16(data[54:], 4) // e_phentsize

		if _, err := InspectArtifactBytes(data); err == nil {
			t.Error("InspectArtifactBytes() with a four-byte program header entry = nil, want an error")
		}
	})

	t.Run("32-bit", func(t *testing.T) {
		data := validELF32(0x03, 1)                 // 386
		binary.LittleEndian.PutUint16(data[42:], 4) // e_phentsize

		if _, err := InspectArtifactBytes(data); err == nil {
			t.Error("InspectArtifactBytes() with a four-byte program header entry = nil, want an error")
		}
	})
}

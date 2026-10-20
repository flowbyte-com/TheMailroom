package mailbox

import (
	"strconv"
	"strings"
)

// MaxPartPathBytes bounds the textual attachment part reference. A
// valid path is a sequence of positive decimal components, so this is
// far larger than any path that can exist within the configured MIME
// nesting and part-count limits.
const MaxPartPathBytes = 128

// ParsePartPath validates an IMAP-style, 1-indexed attachment part
// path and returns its numeric components. It accepts only paths such
// as "1", "2" and "1.2.3"; zero components, leading zeroes, signs,
// whitespace and zero-length components are rejected.
//
// maxDepth bounds the number of components (MIME nesting depth) and
// maxParts bounds each component value (the configured part count).
// Non-positive bounds fall back to conservative defaults so a caller
// that forgets to supply limits cannot turn this into an unbounded
// parser.
func ParsePartPath(part string, maxDepth, maxParts int) ([]int, error) {
	if part == "" || len(part) > MaxPartPathBytes {
		return nil, ErrInvalidPart
	}
	if maxDepth <= 0 {
		maxDepth = 64
	}
	if maxParts <= 0 {
		maxParts = 1_000_000
	}
	fields := strings.Split(part, ".")
	if len(fields) > maxDepth {
		return nil, ErrInvalidPart
	}
	path := make([]int, len(fields))
	for i, field := range fields {
		if field == "" || (len(field) > 1 && field[0] == '0') {
			return nil, ErrInvalidPart
		}
		for _, r := range field {
			if r < '0' || r > '9' {
				return nil, ErrInvalidPart
			}
		}
		n, err := strconv.ParseUint(field, 10, 31)
		if err != nil || n == 0 || n > uint64(maxParts) {
			return nil, ErrInvalidPart
		}
		path[i] = int(n)
	}
	return path, nil
}

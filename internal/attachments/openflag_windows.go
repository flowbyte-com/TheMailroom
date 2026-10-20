//go:build windows

package attachments

// Windows paths do not expose Unix FIFO semantics through os.Root. The
// pre-open regular-file check remains in place.
const nonBlockOpenFlag = 0

//go:build unix

package attachments

import "syscall"

// nonBlockOpenFlag prevents a FIFO swap from blocking the resolver between
// the pre-open stat and the actual open. It has no effect on regular files.
const nonBlockOpenFlag = syscall.O_NONBLOCK

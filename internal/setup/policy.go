package setup

import "errors"

// errWindowsACL explains why setup refuses to write on Windows.
//
// Go's standard library cannot set a Windows access control list, and a
// newly created file inherits the ACL of its directory. A shared directory
// would therefore yield a configuration readable by other users, and a
// warning after the fact would be too late. Setup fails closed instead.
var errWindowsACL = errors.New(
	"setup: on Windows this build cannot restrict a configuration file's access control list, " +
		"so it cannot guarantee the file is readable only by its owner; nothing was written. " +
		"Create the file by hand in a directory only you can read, then restrict it, for example: " +
		"icacls <path> /inheritance:r /grant:r %USERNAME%:F")

// platformWritePolicy reports whether setup can create a configuration file
// with the confidentiality it requires on the given platform.
//
// POSIX systems use mode 0600, which setup enforces regardless of the umask.
// Windows has no equivalent in the standard library, so the policy is to
// fail closed rather than create a file whose access cannot be assured.
func platformWritePolicy(goos string) error {
	if goos == "windows" {
		return errWindowsACL
	}
	return nil
}

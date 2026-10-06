//go:build windows

package updater

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isEnvStartError reports whether a failed StartService is caused by the
// machine/account configuration rather than by the binary being broken.
func isEnvStartError(err error) bool {
	for _, code := range []windows.Errno{
		windows.ERROR_ACCESS_DENIED,           // 5    - we may not start it
		windows.ERROR_SERVICE_LOGON_FAILED,    // 1069 - password changed / logon right revoked
		windows.ERROR_SERVICE_DISABLED,        // 1058 - an admin disabled it
		windows.ERROR_INVALID_SERVICE_ACCOUNT, // 1057
		windows.ERROR_ACCOUNT_RESTRICTION,     // 1327
		windows.ERROR_LOGON_FAILURE,           // 1326
	} {
		if errors.Is(err, code) {
			return true
		}
	}
	return false
}

package trust

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func retryLockCreation(err error) bool {
	// RemoveDirectory can leave the name delete-pending while another handle
	// closes. Only a successful Mkdir admits the caller; genuine permission
	// failures stay bounded by the lock budget and retain their original error.
	return errors.Is(err, os.ErrExist) || errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

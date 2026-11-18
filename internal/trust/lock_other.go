//go:build !windows

package trust

import (
	"errors"
	"os"
)

func retryLockCreation(err error) bool { return errors.Is(err, os.ErrExist) }

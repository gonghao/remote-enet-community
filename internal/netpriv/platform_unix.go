//go:build !windows

package netpriv

import "os"

func isElevated() bool { return os.Geteuid() == 0 }

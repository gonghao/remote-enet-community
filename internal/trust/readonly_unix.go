//go:build !windows

package trust

import (
	"os"
	"syscall"
)

func openPublicSnapshot(root *os.Root) (*os.File, error) {
	// A replacement FIFO must not turn a read-only check into an unbounded open.
	return root.OpenFile("trust.json", os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}

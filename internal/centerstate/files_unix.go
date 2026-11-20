//go:build unix

package centerstate

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

const snapshotFlags = unix.O_NOFOLLOW | unix.O_NONBLOCK

func tryLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EINTR) {
		return false, nil
	}
	return err == nil, err
}

func syncDirectory(dir *os.Root) error {
	f, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

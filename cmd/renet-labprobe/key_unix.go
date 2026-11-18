//go:build unix

package main

import (
	"golang.org/x/sys/unix"
	"os"
)

func openKey(path string) (*os.File, error) {
	before, err := keyInfo(path)
	if err != nil {
		return nil, errKeyFile
	}
	return openKeyChecked(path, before)
}

func openKeyChecked(path string, before os.FileInfo) (*os.File, error) {
	// O_NONBLOCK prevents a replacement FIFO from waiting for a writer.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errKeyFile
	}
	return verifyKey(os.NewFile(uintptr(fd), path), before)
}

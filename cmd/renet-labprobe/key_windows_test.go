package main

import "testing"

func TestKeyRejectsDeviceAndRemotePaths(t *testing.T) {
	for _, path := range []string{`\\server\share\key`, `\\.\pipe\fixture`, `\\?\C:\fixture`, `C:\fixture:secret`, `NUL`, `CON`} {
		assertKeyRejected(t, path)
	}
}

package main

import (
	"errors"
	"io"
	"os"
)

const maxKeyBytes = 65536

var errKeyFile = errors.New("invalid key file")

func readKey(path string) (data []byte, err error) {
	f, err := openKey(path)
	if err != nil {
		return nil, errKeyFile
	}
	defer func() {
		if f.Close() != nil {
			clear(data)
			data = nil
			err = errKeyFile
		}
	}()
	data, err = io.ReadAll(io.LimitReader(f, maxKeyBytes+1))
	if err != nil || len(data) > maxKeyBytes {
		clear(data)
		return nil, errKeyFile
	}
	return data, nil
}

func keyInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxKeyBytes {
		return nil, errKeyFile
	}
	return info, nil
}

func verifyKey(f *os.File, before os.FileInfo) (*os.File, error) {
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || after.Size() > maxKeyBytes || !os.SameFile(before, after) {
		_ = f.Close()
		return nil, errKeyFile
	}
	return f, nil
}

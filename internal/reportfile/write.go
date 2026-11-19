package reportfile

import (
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Write atomically publishes a synced 0600 report. Existing destinations must be regular files.
func Write(path, tempPrefix string, encode func(io.Writer) error) error {
	if s, err := os.Lstat(path); err == nil {
		if !s.Mode().IsRegular() {
			return errors.New("unsafe destination")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Directory aliases are normal on macOS and Windows. Hold the directory
	// handle so temporary files and replacement stay in that same directory.
	dir, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	destination := filepath.Base(path)
	if s, err := dir.Lstat(destination); err == nil {
		if !s.Mode().IsRegular() {
			return errors.New("unsafe destination")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var f *os.File
	var tempName string
	for range 10 {
		tempName = tempPrefix + rand.Text()
		f, err = dir.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if !errors.Is(err, os.ErrExist) {
			break
		}
	}
	if err != nil {
		return err
	}
	defer dir.Remove(tempName)
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if err = encode(f); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if s, err := dir.Lstat(destination); err == nil {
		if !s.Mode().IsRegular() {
			return errors.New("unsafe destination")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return dir.Rename(tempName, destination)
}

//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestKeyFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	assertKeyRejected(t, path)
	if time.Since(start) > time.Second {
		t.Fatal("FIFO rejection exceeded local file budget")
	}
}

func TestKeyReplacement(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink", "regular"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "key")
			if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := keyInfo(path)
			if err != nil {
				t.Fatal(err)
			}
			// Keep the original inode alive so replacement identity cannot be reused.
			if err := os.Rename(path, filepath.Join(dir, "old")); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "fifo":
				err = unix.Mkfifo(path, 0600)
			case "symlink":
				err = os.Symlink(filepath.Join(dir, "old"), path)
			case "regular":
				err = os.WriteFile(path, []byte("replacement"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			f, err := openKeyChecked(path, before)
			if f != nil {
				_ = f.Close()
				t.Fatal("replacement opened")
			}
			if err != errKeyFile {
				t.Fatalf("unexpected error %v", err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("replacement open exceeded local file budget")
			}
		})
	}
}

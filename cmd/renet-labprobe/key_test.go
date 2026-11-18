package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReadKeyLocalFiles(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("fixture-key"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := readKey(regular)
	if err != nil || !bytes.Equal(data, []byte("fixture-key")) {
		t.Fatalf("regular file: %q %v", data, err)
	}
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, make([]byte, maxKeyBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, large, filepath.Join(dir, "missing")} {
		assertKeyRejected(t, path)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(regular, link); err == nil {
		assertKeyRejected(t, link)
	}
}

func assertKeyRejected(t *testing.T, path string) {
	t.Helper()
	data, err := readKey(path)
	if data != nil || err != errKeyFile {
		t.Fatalf("unsafe file returned data=%v error=%v", data, err)
	}
}

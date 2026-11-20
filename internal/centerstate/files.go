package centerstate

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

func openSubdir(parent *os.Root, name string, create bool) (*os.Root, error) {
	if create {
		switch err := parent.Mkdir(name, 0o700); {
		case err == nil:
			if err := syncDirectory(parent); err != nil {
				return nil, fmt.Errorf("sync parent after creating %q: %w", name, err)
			}
		case !errors.Is(err, os.ErrExist):
			return nil, fmt.Errorf("create directory %q: %w", name, err)
		}
	}
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("inspect directory %q: %w", name, err)
	}
	if info.Mode().Type() != os.ModeDir {
		return nil, fmt.Errorf("%q must be a directory without symlinks", name)
	}
	dir, err := parent.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("open directory %q: %w", name, err)
	}
	opened, err := dir.Stat(".")
	if err != nil {
		dir.Close()
		return nil, fmt.Errorf("inspect open directory %q: %w", name, err)
	}
	if !os.SameFile(info, opened) {
		dir.Close()
		return nil, fmt.Errorf("directory %q changed while opening", name)
	}
	return dir, nil
}

// Keep the lock's directory entry after release so all processes use one inode.
func acquireLock(ctx context.Context, root *os.Root) (*os.File, error) {
	const lockName = ".lock"
	if info, err := root.Lstat(lockName); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("state lock must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect state lock: %w", err)
	}
	f, err := root.OpenFile(lockName, os.O_CREATE|os.O_RDWR|snapshotFlags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, fmt.Errorf("wait for state lock: %w", err)
		}
		acquired, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("acquire state lock: %w", err)
		}
		if acquired {
			return f, nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func replaceRecord(ctx context.Context, dir *os.Root, name string, data []byte) error {
	temp := ".state-" + rand.Text() + ".tmp"
	f, err := dir.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary record: %w", err)
	}
	defer dir.Remove(temp)
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write temporary record: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync temporary record: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temporary record: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("commit record: %w", err)
	}
	if err := dir.Rename(temp, name); err != nil {
		return fmt.Errorf("replace record: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync record directory after replacement: %w", err)
	}
	return nil
}

func deleteRecord(ctx context.Context, dir *os.Root, name string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("delete record: %w", err)
	}
	err := dir.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove record: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync record directory after deletion: %w", err)
	}
	return nil
}

// Base64url filenames are case-sensitive even on case-insensitive filesystems.
func checkPinName(dir *os.Root, name string) error {
	if _, err := dir.Lstat(name); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect pin filename: %w", err)
	}
	entries, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		return fmt.Errorf("list pin filenames: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == name {
			return nil
		}
	}
	return fmt.Errorf("pin filename %q collides with an existing entry", name)
}

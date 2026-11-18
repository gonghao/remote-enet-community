package trust

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestLockWaitsForDeletePendingDirectory(t *testing.T) {
	s := openStore(t)
	path := s.path + ".lock"
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if handle != windows.InvalidHandle {
			_ = windows.CloseHandle(handle)
		}
	}()
	deleteFile := uint32(1)
	if err := windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo, (*byte)(unsafe.Pointer(&deleteFile)), uint32(unsafe.Sizeof(deleteFile))); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("expected delete-pending denial: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	_, err = s.lock(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("lost cancellation or original error: %v", err)
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	handle = windows.InvalidHandle
	unlock, err := s.lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func openKey(path string) (*os.File, error) {
	path, err := filepath.Abs(path)
	if err != nil || len(path) < 3 || path[1] != ':' || path[2] != '\\' || !((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) || strings.Contains(path[2:], ":") || !filepath.IsLocal(path[3:]) {
		return nil, errKeyFile
	}
	root, err := windows.UTF16PtrFromString(path[:3])
	if err != nil {
		return nil, errKeyFile
	}
	drive := windows.GetDriveType(root)
	if drive != windows.DRIVE_FIXED && drive != windows.DRIVE_REMOVABLE && drive != windows.DRIVE_RAMDISK {
		return nil, errKeyFile
	}
	// Reject reparse-point parents before inspecting the final local file.
	current := path[:3]
	for _, part := range strings.Split(filepath.Dir(path)[3:], `\`) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errKeyFile
		}
	}
	before, err := keyInfo(path)
	if err != nil {
		return nil, errKeyFile
	}
	// Windows Lstat loads file identity lazily; capture it before denying replacement.
	if !os.SameFile(before, before) {
		return nil, errKeyFile
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, errKeyFile
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, errKeyFile
	}
	f := os.NewFile(uintptr(handle), path)
	var info windows.ByHandleFileInformation
	kind, typeErr := windows.GetFileType(handle)
	if windows.GetFileInformationByHandle(handle, &info) != nil || typeErr != nil || kind != windows.FILE_TYPE_DISK || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		_ = f.Close()
		return nil, errKeyFile
	}
	return verifyKey(f, before)
}

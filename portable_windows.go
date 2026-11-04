//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func tryPortableLock(file *os.File) error {
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

func closePortableLock(file *os.File) {
	// Windows can release a lock asynchronously if the handle is just closed.
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
	file.Close()
}

func openPortableBrowser(url string) error {
	address, err := windows.UTF16PtrFromString(url)
	if err != nil {
		return err
	}
	// Pass the HTTP URL directly to its registered Windows handler.
	return windows.ShellExecute(0, nil, address, nil, nil, windows.SW_SHOWNORMAL)
}

//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func tryPortableLock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func closePortableLock(file *os.File) { file.Close() }

// Linux portable releases print the server's LAN address for the DBA.
func openPortableBrowser(string) error { return nil }

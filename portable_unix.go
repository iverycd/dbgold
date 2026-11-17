//go:build !windows

package main

import (
	"os"
	"os/exec"
	"runtime"

	"golang.org/x/sys/unix"
)

func tryPortableLock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func closePortableLock(file *os.File) { file.Close() }

func openPortableBrowser(url string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("/usr/bin/open", url).Run()
	}
	return nil
}

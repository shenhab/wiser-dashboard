//go:build !windows

package main

import (
	"os"
	"syscall"
)

// relaunch replaces this process with the (updated) program, keeping its
// arguments, environment and process ID, so a supervisor like launchd keeps
// tracking it.
func relaunch(exe string) error {
	return syscall.Exec(exe, os.Args, os.Environ())
}

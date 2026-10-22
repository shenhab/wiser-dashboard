//go:build windows

package main

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// relaunch starts the (updated) program minimized with the same arguments;
// the caller then exits. Windows has no exec, and "start /min" gives the new
// console window the same state the startup shortcut does.
func relaunch(exe string) error {
	parts := []string{"cmd.exe", "/c", "start", `""`, "/min", syscall.EscapeArg(exe)}
	for _, a := range os.Args[1:] {
		parts = append(parts, syscall.EscapeArg(a))
	}
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: strings.Join(parts, " "), HideWindow: true}
	return cmd.Start()
}

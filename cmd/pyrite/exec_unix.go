package main

import "os/exec"

// execCmd starts a detached command, used only to open a browser window.
func execCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Start()
}

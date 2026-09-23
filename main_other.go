//go:build !windows

package main

import "os/exec"

func ownConsole() bool { return false }

func openBrowser(url string) {
	_ = exec.Command("xdg-open", url).Start()
}

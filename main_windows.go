package main

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	shell32                   = syscall.NewLazyDLL("shell32.dll")
	procShellExecuteW         = shell32.NewProc("ShellExecuteW")
)

// ownConsole reports that kanshi.exe has a console window to itself, which is
// what happens when it is double-clicked in Explorer rather than started from
// a terminal (where the shell shares the console).
func ownConsole() bool {
	var ids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&ids[0])), 2)
	return n == 1
}

func openBrowser(url string) {
	verb, _ := syscall.UTF16PtrFromString("open")
	target, _ := syscall.UTF16PtrFromString(url)
	const swShowNormal = 1
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(target)), 0, 0, swShowNormal)
}

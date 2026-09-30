//go:build windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// 免回显读密码。为保持「纯标准库、零依赖」，这里不进 x/term，
// 直接用 kernel32 关掉控制台的 ENABLE_ECHO_INPUT。

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetStdHandle   = kernel32.NewProc("GetStdHandle")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

const (
	stdInputHandle  = ^uintptr(9) // -10，即 STD_INPUT_HANDLE
	enableEchoInput = 0x0004
)

func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	h, _, _ := procGetStdHandle.Call(stdInputHandle)

	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode))); r == 0 {
		// stdin 不是控制台（被管道/重定向）——没法关回显，直接读一行。
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}

	procSetConsoleMode.Call(h, uintptr(mode&^enableEchoInput))
	defer func() {
		procSetConsoleMode.Call(h, uintptr(mode))
		fmt.Fprintln(os.Stderr)
	}()

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

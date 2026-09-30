//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// 免回显读密码。纯标准库：用 TCGETS/TCSETS 关掉 ECHO 位。

type termios struct {
	Iflag  uint32
	Oflag  uint32
	Cflag  uint32
	Lflag  uint32
	Line   uint8
	Cc     [32]uint8
	Ispeed uint32
	Ospeed uint32
}

const (
	tcgets = 0x5401
	tcsets = 0x5402
	echo   = 0x00000008
)

func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	fd := os.Stdin.Fd()

	var old termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, tcgets,
		uintptr(unsafe.Pointer(&old)), 0, 0, 0)
	if errno != 0 {
		// 非终端（管道/重定向）——直接读一行。
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}

	noEcho := old
	noEcho.Lflag &^= echo
	syscall.Syscall6(syscall.SYS_IOCTL, fd, tcsets,
		uintptr(unsafe.Pointer(&noEcho)), 0, 0, 0)
	defer func() {
		syscall.Syscall6(syscall.SYS_IOCTL, fd, tcsets,
			uintptr(unsafe.Pointer(&old)), 0, 0, 0)
		fmt.Fprintln(os.Stderr)
	}()

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

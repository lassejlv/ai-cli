//go:build !windows

package output

import (
	"os"

	"golang.org/x/sys/unix"
)

func terminalSize() (cols, xpixel int) {
	for _, f := range []*os.File{os.Stderr, os.Stdout, os.Stdin} {
		ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
		if err == nil && ws.Col > 0 {
			return int(ws.Col), int(ws.Xpixel)
		}
	}
	return 0, 0
}

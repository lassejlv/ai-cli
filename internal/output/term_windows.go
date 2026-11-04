//go:build windows

package output

import (
	"os"

	"golang.org/x/term"
)

func terminalSize() (cols, xpixel int) {
	w, _, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil {
		return 0, 0
	}
	return w, 0
}

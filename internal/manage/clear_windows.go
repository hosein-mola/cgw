//go:build windows

package manage

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func clearScreen() error {
	handle := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err == nil {
		_ = windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
	_, err := fmt.Fprint(os.Stdout, "\x1b[2J\x1b[H")
	return err
}

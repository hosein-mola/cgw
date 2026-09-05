//go:build !windows

package manage

import (
	"fmt"
	"os"
)

func clearScreen() error {
	_, err := fmt.Fprint(os.Stdout, "\x1b[2J\x1b[H")
	return err
}

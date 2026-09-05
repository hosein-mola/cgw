//go:build !windows

package manage

import (
	"os"
	"os/exec"
	"syscall"
)

func protect(path string, dir bool) error {
	mode := os.FileMode(0600)
	if dir {
		mode = 0700
	}
	return os.Chmod(path, mode)
}
func seal(b []byte) ([]byte, error)     { return b, nil }
func unseal(b []byte) ([]byte, error)   { return b, nil }
func replaceFile(src, dst string) error { return os.Rename(src, dst) }
func detach(cmd *exec.Cmd)              { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

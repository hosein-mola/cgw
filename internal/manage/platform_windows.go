package manage

import (
	"golang.org/x/sys/windows"
	"os/exec"
	"os/user"
	"syscall"
	"unsafe"
)

func protect(path string, dir bool) error {
	u, err := user.Current()
	if err != nil {
		return err
	}
	access := "D:P(A;;FA;;;" + u.Uid + ")"
	if dir {
		access = "D:P(A;OICI;FA;;;" + u.Uid + ")"
	}
	sd, err := windows.SecurityDescriptorFromString(access)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
func replaceFile(src, dst string) error {
	a, e := windows.UTF16PtrFromString(src)
	if e != nil {
		return e
	}
	b, e := windows.UTF16PtrFromString(dst)
	if e != nil {
		return e
	}
	return windows.MoveFileEx(a, b, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000200 | 0x00000008}
}

func seal(b []byte) ([]byte, error)   { return crypt(b, true) }
func unseal(b []byte) ([]byte, error) { return crypt(b, false) }
func crypt(b []byte, encrypt bool) ([]byte, error) {
	if len(b) == 0 {
		return nil, nil
	}
	in := windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
	var out windows.DataBlob
	var err error
	if encrypt {
		err = windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	if err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(out.Data))))
	return append([]byte(nil), unsafe.Slice(out.Data, int(out.Size))...), nil
}

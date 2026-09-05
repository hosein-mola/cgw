package manage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func privateDir(path string) error {
	if err := noLinks(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return protect(path, true)
}

// Refuse symlinks throughout the destination path, including existing parents.
func noLinks(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for {
		info, e := os.Lstat(path)
		if e == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink: %s", path)
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	return nil
}

func atomicWrite(path string, b []byte) error {
	if err := noLinks(path); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".proxy-write-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = protect(temp, false); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return replaceFile(temp, path)
}
func writeJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return atomicWrite(path, append(b, '\n'))
}
func readJSON(path string, v any) error {
	if err := noLinks(path); err != nil {
		return err
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}

func lock(path string) (func(), error) {
	if err := noLinks(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("operation locked (%s); if no management command is running, remove this lock and retry: %w", path, err)
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Close()
	return func() { _ = os.Remove(path) }, nil
}

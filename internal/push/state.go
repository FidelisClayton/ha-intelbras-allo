package push

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func privateParent(path string, create bool) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(path)
	var check func(string) error
	check = func(p string) error {
		if p != filepath.Dir(p) {
			if err := check(filepath.Dir(p)); err != nil {
				return err
			}
		}
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) && create {
			if err = os.Mkdir(p, 0700); err != nil {
				return err
			}
			info, err = os.Lstat(p)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe private directory")
		}
		return nil
	}
	if err := check(parent); err != nil {
		return "", err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return "", err
	}
	if info.Mode().Perm() != 0700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return "", errors.New("private directory must be 0700")
	}
	return path, nil
}

func readPrivate(path string, limit int64, value any) error {
	b, err := readPrivateBytes(path, limit)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}

func readPrivateBytes(path string, limit int64) ([]byte, error) {
	path, err := privateParent(path, false)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > limit || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return nil, errors.New("unsafe private file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("private file too large")
	}
	return b, nil
}

func savePrivate(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil || len(b) > 1<<20 {
		return errors.New("private state encoding failed")
	}
	path, err = privateParent(path, true)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return errors.New("unsafe private state")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".push-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

package fsutil

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

var ErrUnsafe = errors.New("unsafe filesystem object")
var ErrJSON = errors.New("invalid JSON")

func regular(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s", ErrUnsafe, path)
	}
	return nil
}

func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrUnsafe
	}
	return f.Sync()
}

func WriteJSON(path string, data []byte) error {
	if !json.Valid(data) {
		return ErrJSON
	}
	return Write(path, data)
}

func Write(path string, data []byte) (result error) {
	if err := regular(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".atomic-")
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			result = errors.Join(result, err)
		}
		if err := os.Remove(f.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}()
	if err := f.Chmod(0644); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return SyncDir(dir)
}

func SHA256(path string) (string, error) {
	if err := regular(path); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func VerifyJar(path, expected string) error {
	hash, err := SHA256(path)
	if err != nil {
		return err
	}
	if hash != expected {
		return fmt.Errorf("%w: checksum %s", ErrUnsafe, path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 1000 || info.Mode().Perm() != 0644 {
		return fmt.Errorf("%w: owner/mode %s", ErrUnsafe, path)
	}
	return nil
}

func Move(src, dst string) error {
	if err := regular(src); err != nil {
		return err
	}
	if err := regular(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	return errors.Join(SyncDir(filepath.Dir(dst)), SyncDir(filepath.Dir(src)))
}

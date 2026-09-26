//go:build !windows

package operations

import (
	"os"
	"path/filepath"
)

func restrictPath(path string, directory bool) error {
	mode := os.FileMode(0o600)
	if directory {
		mode = 0o700
	}
	return os.Chmod(path, mode)
}

func makeSecureTempDir(pattern string) (string, error) {
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", err
	}
	if err := restrictPath(dir, true); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func createSecureTempFile(dir, pattern string) (*os.File, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := restrictPath(file.Name(), false); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}

func createSecureFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

func durableRename(source, destination string) error {
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

func replaceFile(source, destination string) error {
	return durableRename(source, destination)
}

func durableRemove(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

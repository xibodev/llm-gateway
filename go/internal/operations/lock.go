package operations

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"llmgw/internal/config"
)

var ErrStateInUse = errors.New("gateway state is in use; stop the gateway before maintenance")

type StateLock struct {
	file *os.File
}

func AcquireStateLock() (*StateLock, error) {
	dir := config.StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, ".state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		return nil, ErrStateInUse
	}
	return &StateLock{file: file}, nil
}

func (lock *StateLock) Release() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unlockFile(lock.file)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}

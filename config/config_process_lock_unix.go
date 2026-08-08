//go:build !windows

package config

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

type unixConfigProcessLock struct {
	file *os.File
}

func acquireConfigProcessLock(identity string) (configProcessLock, error) {
	path := configLockFilePath(identity)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrConfigPathInUse
		}
		return nil, fmt.Errorf("flock %q: %w", path, err)
	}
	return &unixConfigProcessLock{file: file}, nil
}

func (l *unixConfigProcessLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

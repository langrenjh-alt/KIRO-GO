//go:build windows

package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

const (
	waitObject0 = 0x00000000
	waitTimeout = 0x00000102
	waitFailed  = 0xffffffff
)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procCreateSemaphoreW = kernel32.NewProc("CreateSemaphoreW")
	procWaitForSingleObj = kernel32.NewProc("WaitForSingleObject")
	procReleaseSemaphore = kernel32.NewProc("ReleaseSemaphore")
)

type windowsConfigProcessLock struct {
	handle syscall.Handle
}

func acquireConfigProcessLock(identity string) (configProcessLock, error) {
	digest := sha256.Sum256([]byte(strings.ToLower(identity)))
	name, err := syscall.UTF16PtrFromString("Global\\kiro-go-config-" + hex.EncodeToString(digest[:]))
	if err != nil {
		return nil, err
	}
	// A semaphore is process-safe without the thread ownership constraint of a
	// Windows mutex. That matters because a Go goroutine can resume on a
	// different OS thread before Close is called.
	handle, _, callErr := procCreateSemaphoreW.Call(0, 1, 1, uintptr(unsafe.Pointer(name)))
	if handle == 0 {
		if callErr == nil {
			callErr = syscall.EINVAL
		}
		return nil, fmt.Errorf("CreateSemaphoreW: %w", callErr)
	}

	waitResult, _, waitErr := procWaitForSingleObj.Call(handle, 0)
	switch uint32(waitResult) {
	case waitObject0:
		return &windowsConfigProcessLock{handle: syscall.Handle(handle)}, nil
	case waitTimeout:
		_ = syscall.CloseHandle(syscall.Handle(handle))
		return nil, ErrConfigPathInUse
	case waitFailed:
		_ = syscall.CloseHandle(syscall.Handle(handle))
		if waitErr == nil {
			waitErr = syscall.EINVAL
		}
		return nil, fmt.Errorf("WaitForSingleObject: %w", waitErr)
	default:
		_ = syscall.CloseHandle(syscall.Handle(handle))
		return nil, fmt.Errorf("WaitForSingleObject returned 0x%x", waitResult)
	}
}

func (l *windowsConfigProcessLock) Close() error {
	if l == nil || l.handle == 0 {
		return nil
	}
	handle := l.handle
	l.handle = 0
	var releaseErr error
	if ok, _, err := procReleaseSemaphore.Call(uintptr(handle), 1, 0); ok == 0 {
		releaseErr = err
		if releaseErr == nil {
			releaseErr = syscall.EINVAL
		}
	}
	closeErr := syscall.CloseHandle(handle)
	if releaseErr != nil {
		return fmt.Errorf("ReleaseSemaphore: %w", releaseErr)
	}
	return closeErr
}

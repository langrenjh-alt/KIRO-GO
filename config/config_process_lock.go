package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrConfigPathInUse indicates that another process already owns the
// configuration path. Config is persisted as one JSON document, so allowing
// two processes to write it would let an older in-memory snapshot erase newer
// API-key bindings.
var ErrConfigPathInUse = errors.New("configuration path is already in use")

type configProcessLock interface {
	Close() error
}

// configPathIdentity returns a stable identity for the config file. Resolving
// the parent directory prevents relative and absolute spellings from creating
// separate locks while still working before the config file itself exists.
func configPathIdentity(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("configuration path must not be empty")
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve configuration path: %w", err)
	}
	dir := filepath.Dir(abs)
	if resolvedDir, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolvedDir
	}
	return filepath.Join(dir, filepath.Base(abs)), nil
}

func sameConfigPathIdentity(a, b string) bool {
	if filepath.Separator == '\\' {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func configPathLockError(path string, err error) error {
	if errors.Is(err, ErrConfigPathInUse) {
		return fmt.Errorf("%w: %s", ErrConfigPathInUse, path)
	}
	return fmt.Errorf("acquire configuration lock for %q: %w", path, err)
}

// Keep the lock file on Unix. Removing it after unlocking would introduce a
// race where a third process creates a replacement path while the old lock is
// still held by a second process.
func configLockFilePath(identity string) string {
	return identity + ".lock"
}

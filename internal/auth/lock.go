package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// withFileLock runs fn while holding <path>.lock, a lock shared across
// processes. A lock file older than staleAfter is treated as abandoned.
func withFileLock(path string, fn func() error) error {
	const staleAfter = 2 * time.Minute
	lock := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if st, statErr := os.Stat(lock); statErr == nil && time.Since(st.ModTime()) > staleAfter {
			_ = os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s; delete it if no other ai process is running", lock)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer os.Remove(lock)
	return fn()
}

// writeFileAtomic writes data to a unique temp file and renames it into place.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

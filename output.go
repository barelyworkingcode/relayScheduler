package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// maxOutputBytes caps a PTY task's output file. History keeps 100 runs and is
// rewritten whole on each run, so the cap bounds one task's history size.
const maxOutputBytes = 64 * 1024

// readOutputFile reads the file a PTY task wrote as its structured output.
// It opens with O_NOFOLLOW so a symlinked last component is refused, and
// O_NONBLOCK so a FIFO can't block the open before the regular-file check.
// A file last modified before notBefore (truncated to the second, for
// filesystems with coarse mtimes) was not written by this run.
func readOutputFile(dir, name string, notBefore time.Time) (string, error) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		switch {
		case errors.Is(err, syscall.ELOOP):
			return "", errors.New("output file is a symlink")
		case errors.Is(err, os.ErrNotExist):
			return "", errors.New("output file not produced")
		}
		return "", fmt.Errorf("output file unreadable: %w", unwrapPathError(err))
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("output file unreadable: %w", unwrapPathError(err))
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("output file is not a regular file")
	}
	if info.ModTime().Before(notBefore.Truncate(time.Second)) {
		return "", errors.New("output file not produced")
	}
	if info.Size() > maxOutputBytes {
		return "", errors.New("output file is over the 64 KB cap")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxOutputBytes+1))
	if err != nil {
		return "", fmt.Errorf("output file unreadable: %w", unwrapPathError(err))
	}
	if len(data) > maxOutputBytes {
		return "", errors.New("output file is over the 64 KB cap")
	}
	return string(data), nil
}

// unwrapPathError drops the path from an *os.PathError so the message carries
// only the OS error.
func unwrapPathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

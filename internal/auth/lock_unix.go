//go:build unix

package auth

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Lock takes an exclusive lock on the account's token files, shared with
// every other `email` process (the watch sidecar and the agent's commands
// refresh the same tokens). The returned function releases it. The lock
// is advisory (flock) and is dropped by the kernel if the process dies.
// Waiting for it ends with ctx.
func (s *Store) Lock(ctx context.Context, alias string) (func(), error) {
	if err := ValidateAlias(alias); err != nil {
		return nil, err
	}
	dir := s.AccountDir(alias)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

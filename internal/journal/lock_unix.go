//go:build linux || darwin || freebsd || netbsd || openbsd

package journal

import (
	"errors"
	"os"
	"syscall"
)

func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errLockHeld
	}
	return err
}

//go:build !linux && !darwin && !freebsd && !windows

package journal

func DiskFree(string) (uint64, error) { return 0, ErrDiskUnsupported }

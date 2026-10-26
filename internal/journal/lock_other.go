//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !windows

package journal

import "os"

// Sin soporte de candado en esta plataforma: no se impide la doble instancia (limitación documentada).
func tryLock(*os.File) error { return nil }

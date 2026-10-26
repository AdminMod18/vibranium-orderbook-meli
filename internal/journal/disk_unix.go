//go:build linux || darwin || freebsd

package journal

import "syscall"

// DiskFree devuelve los bytes que un proceso sin privilegios puede escribir aún en el sistema de archivos
// que contiene dir (si se llena, el journal no puede escribir y el servicio hace fail-stop).
func DiskFree(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

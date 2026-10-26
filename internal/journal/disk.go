package journal

import "errors"

// ErrDiskUnsupported: esta plataforma no sabe informar el espacio libre.
var ErrDiskUnsupported = errors.New("espacio en disco no soportado en esta plataforma")

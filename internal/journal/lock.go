package journal

import (
	"errors"
	"fmt"
	"os"
)

// ErrLocked indica que OTRA instancia ya tiene abierto este journal.
//
// Dos procesos escribiendo el mismo journal lo corromperían (cada uno lleva su propio contador de
// secuencia y su propia posición de escritura). El candado es del sistema operativo (flock / LockFileEx):
// se libera solo cuando el proceso muere, incluso con kill -9, así que NUNCA deja un candado huérfano.
var ErrLocked = errors.New("el journal ya está en uso por otra instancia")

// acquireLock toma el candado exclusivo del journal sin bloquearse. Devuelve el archivo que lo sostiene:
// mientras esté abierto, el candado es nuestro.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		f.Close()
		if errors.Is(err, errLockHeld) {
			return nil, fmt.Errorf("%w (candado %s): ¿hay otro servidor corriendo con el mismo JOURNAL_PATH?", ErrLocked, path)
		}
		return nil, err
	}
	return f, nil
}

var errLockHeld = errors.New("candado ocupado")

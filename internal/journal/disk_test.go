package journal

import (
	"errors"
	"testing"
)

func TestDiskFreeReportsSomethingSensible(t *testing.T) {
	free, err := DiskFree(t.TempDir())
	if errors.Is(err, ErrDiskUnsupported) {
		t.Skip("plataforma sin soporte")
	}
	if err != nil || free == 0 {
		t.Fatalf("debía informar espacio libre > 0: %d %v", free, err)
	}
	if _, err := DiskFree("/ruta/que/no/existe/jamas"); err == nil {
		t.Fatal("una ruta inexistente debe dar error")
	}
}

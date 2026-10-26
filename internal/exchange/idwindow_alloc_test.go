package exchange

import (
	"fmt"
	"runtime"
	"testing"
)

// Hipótesis RECHAZADA con este test: el perfil de memoria mostró idWindow.add con ≈ 20 % de la basura, pero ese gasto
// venía de reconstruir la ventana al ARRANCAR (Restore inserta millones de ids). En régimen estacionario (inserta uno y
// expulsa uno) add() reserva ≈ 42 B por orden, despreciable frente a los ≈ 25 KB por orden del resto del servidor.
// El test fija una cota para que una regresión (p. ej. un mapa que se reconstruya) no pase inadvertida.
func TestIDWindowSteadyStateAllocation(t *testing.T) {
	const window, extra = 200_000, 400_000
	w := newIDWindow(window)
	ids := make([]string, window+extra)
	for i := range ids {
		ids[i] = fmt.Sprintf("lg100276201-%d", i)
	}
	for i := 0; i < window; i++ {
		w.add(ids[i])
	}
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	for i := window; i < window+extra; i++ {
		w.add(ids[i])
	}
	runtime.ReadMemStats(&m1)
	per := float64(m1.TotalAlloc-m0.TotalAlloc) / extra
	if per > 400 {
		t.Fatalf("add() reserva %.0f B por inserción en régimen estacionario (medido: ≈ 42 B)", per)
	}
	t.Logf("add() en régimen estacionario: %.1f B por inserción (%d MiB en %d inserciones, evicted=%d)", per,
		(m1.TotalAlloc-m0.TotalAlloc)>>20, extra, w.evicted.Load())
}

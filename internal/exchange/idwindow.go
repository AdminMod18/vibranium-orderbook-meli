package exchange

import "sync/atomic"

// idWindow recuerda los ÚLTIMOS `limit` ids de órdenes aceptadas (idempotencia con ventana).
//
// La ventana se mide por CANTIDAD de órdenes aceptadas, no por tiempo: así depende solo de la secuencia
// de comandos y no del reloj, y reproducir el journal reconstruye exactamente lo mismo (determinismo).
// limit <= 0 significa sin límite (crece para siempre; solo para pruebas).
//
// No es seguro para uso concurrente: lo usa el goroutine secuenciador. Los contadores atómicos
// existen únicamente para que las métricas puedan leerlos desde otros goroutines.
type idWindow struct {
	limit int
	set   map[string]struct{}
	queue []string // de más antiguo a más nuevo; los vigentes son queue[head:]
	head  int

	size    atomic.Int64
	evicted atomic.Uint64
}

func newIDWindow(limit int) *idWindow {
	return &idWindow{limit: limit, set: make(map[string]struct{})}
}

func (w *idWindow) has(id string) bool { _, ok := w.set[id]; return ok }

// add recuerda un id y expulsa los más antiguos si se superó el límite. Si ya estaba, no hace nada
// (ocurre al reproducir el journal con una ventana mayor que la de la ejecución original).
func (w *idWindow) add(id string) {
	if _, ok := w.set[id]; ok {
		return
	}
	w.set[id] = struct{}{}
	w.queue = append(w.queue, id)
	for w.limit > 0 && len(w.set) > w.limit {
		old := w.queue[w.head]
		w.queue[w.head] = "" // libera la cadena para el GC
		w.head++
		delete(w.set, old)
		w.evicted.Add(1)
	}
	// Compacta la cola cuando más de la mitad es espacio ya expulsado (costo amortizado O(1)).
	if w.head >= 1<<14 && w.head*2 >= len(w.queue) {
		n := copy(w.queue, w.queue[w.head:])
		for i := n; i < len(w.queue); i++ {
			w.queue[i] = ""
		}
		w.queue = w.queue[:n]
		w.head = 0
	}
	w.size.Store(int64(len(w.set)))
}

// items devuelve una copia de los ids vigentes, del más antiguo al más nuevo.
// Es un simple copiado de memoria (no recorre el mapa): la pausa del snapshot casi no crece con la ventana.
func (w *idWindow) items() []string {
	out := make([]string, len(w.set))
	copy(out, w.queue[w.head:])
	return out
}

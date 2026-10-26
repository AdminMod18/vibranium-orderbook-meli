// loadgen: generador de carga para el order book.
//
// Genera órdenes aleatorias (compras y ventas alrededor de un precio medio, para que crucen) a un
// ritmo objetivo, mide la latencia y al final verifica con /audit que los saldos siguen cuadrando.
//
// La latencia se mide desde el instante en que la request DEBIÓ salir (modelo "open loop"), no desde
// que salió: así, si el servidor se atrasa, el retraso se refleja y no se oculta (coordinated omission).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type sample struct {
	us   int64
	code int
}

func main() {
	var (
		url      = flag.String("url", envOr("LOADGEN_URL", "http://localhost:8080"), "URL base de la API (env LOADGEN_URL)")
		rate     = flag.Int("rate", 2000, "órdenes por segundo objetivo (0 = lo máximo posible, bucle cerrado)")
		duration = flag.Duration("duration", 20*time.Second, "duración de la prueba")
		workers  = flag.Int("workers", 128, "conexiones/goroutines concurrentes")
		users    = flag.Int("users", 100, "cantidad de usuarios simulados")
		cancel   = flag.Float64("cancel", 0.05, "fracción de órdenes que se cancelan luego")
		mid      = flag.Int("mid", 10000, "precio medio en centavos")
		spread   = flag.Int("spread", 40, "dispersión del precio alrededor del medio")
	)
	flag.Parse()

	runID := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{MaxIdleConns: *workers * 2, MaxIdleConnsPerHost: *workers * 2,
			IdleConnTimeout: 60 * time.Second},
	}
	post := func(path string, body any) (int, []byte, error) {
		b, _ := json.Marshal(body)
		resp, err := client.Post(*url+path, "application/json", bytes.NewReader(b))
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, data, nil
	}

	fmt.Printf("Sembrando %d usuarios... ", *users)
	for i := 0; i < *users; i++ {
		code, _, err := post("/deposits", map[string]any{"user_id": fmt.Sprintf("lg%s-u%d", runID, i), "brl": 1_000_000_000_000, "vib": 1_000_000_000})
		if err != nil || code != http.StatusNoContent {
			fmt.Printf("\nno se pudo sembrar (code=%d err=%v). ¿Está la API arriba en %s?\n", code, err, *url)
			os.Exit(2)
		}
	}
	fmt.Println("ok")

	type job struct {
		i  int
		at time.Time // cuándo DEBIÓ salir
	}
	jobs := make(chan job, 20_000)
	results := make([][]sample, *workers)
	var cancels, cancelErr atomic.Int64
	var wg sync.WaitGroup

	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(w)))
			for j := range jobs {
				side := "BUY"
				if rng.Intn(2) == 0 {
					side = "SELL"
				}
				id := fmt.Sprintf("lg%s-%d", runID, j.i)
				code, body, err := post("/orders", map[string]any{
					"id": id, "user_id": fmt.Sprintf("lg%s-u%d", runID, rng.Intn(*users)), "side": side,
					"price": *mid + rng.Intn(2**spread+1) - *spread, "qty": 1 + rng.Intn(10)})
				if err != nil {
					code = -1
				}
				results[w] = append(results[w], sample{us: time.Since(j.at).Microseconds(), code: code})

				if code == http.StatusCreated && rng.Float64() < *cancel && !strings.Contains(string(body), `"status":"FILLED"`) {
					req, _ := http.NewRequest(http.MethodDelete, *url+"/orders/"+id, nil)
					if resp, err := client.Do(req); err == nil {
						resp.Body.Close()
						cancels.Add(1)
						if resp.StatusCode != 200 {
							cancelErr.Add(1)
						}
					}
				}
			}
		}(w)
	}

	start := time.Now()
	deadline := start.Add(*duration)
	fmt.Printf("Carga: objetivo=%d órd/s, %d workers, %s...\n", *rate, *workers, *duration)
	sent := 0
	for now := time.Now(); now.Before(deadline); now = time.Now() {
		at := now
		if *rate > 0 {
			at = start.Add(time.Duration(float64(sent) / float64(*rate) * float64(time.Second)))
			if d := time.Until(at); d > 0 {
				time.Sleep(d)
			}
		}
		// Envío bloqueante: si los workers no dan abasto el calendario se atrasa, pero ninguna orden se
		// pierde y la latencia (medida desde `at`) lo refleja: eso es lo que evita la "omisión coordinada".
		jobs <- job{i: sent, at: at}
		sent++
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)

	// --- resumen ---
	var all []int64
	codes := map[int]int{}
	for _, rs := range results {
		for _, s := range rs {
			all = append(all, s.us)
			codes[s.code]++
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	pct := func(p float64) time.Duration {
		if len(all) == 0 {
			return 0
		}
		return time.Duration(all[int(float64(len(all)-1)*p)]) * time.Microsecond
	}
	fmt.Println("\n=== Resultado ===")
	fmt.Printf("órdenes completadas : %d en %s  =>  %.0f órdenes/s efectivas\n", len(all), elapsed.Round(time.Millisecond), float64(len(all))/elapsed.Seconds())
	if late := elapsed - *duration; late > time.Second {
		fmt.Printf("AVISO: el generador terminó %s después de lo previsto: el sistema no sostuvo el ritmo pedido\n", late.Round(time.Second))
	}
	fmt.Printf("latencia            : p50=%s  p90=%s  p99=%s  p99.9=%s  max=%s\n", pct(.5), pct(.9), pct(.99), pct(.999), pct(1))
	var keys []int
	for k := range codes {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	unexpected := 0
	fmt.Print("respuestas          :")
	for _, k := range keys {
		fmt.Printf("  %d×%d", k, codes[k])
		if k != 201 && k != 422 && k != 503 { // 422 = sin saldo, 503 = backpressure: comportamientos esperados
			unexpected += codes[k]
		}
	}
	fmt.Printf("\ncancelaciones       : %d (ya ejecutadas/no encontradas: %d)\n", cancels.Load(), cancelErr.Load())

	resp, err := client.Get(*url + "/audit")
	if err != nil {
		fmt.Println("auditoría           : no se pudo consultar:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("auditoría de saldos : %s", strings.TrimSpace(string(body)))
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"ok":true`) {
		fmt.Println("\nFALLO: los invariantes NO se cumplen")
		os.Exit(1)
	}
	if unexpected > 0 {
		fmt.Printf("\nFALLO: %d respuestas inesperadas (errores de red o 5xx distintos de 503)\n", unexpected)
		os.Exit(1)
	}
	fmt.Println("\nOK")
}

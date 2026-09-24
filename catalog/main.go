// Mock upstream "catalog" service shared by all implementations.
package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	latency := time.Duration(0)
	if v, err := strconv.Atoi(os.Getenv("LATENCY_MS")); err == nil {
		latency = time.Duration(v) * time.Millisecond
	}
	http.HandleFunc("GET /products/{sku}", func(w http.ResponseWriter, r *http.Request) {
		if latency > 0 {
			time.Sleep(latency)
		}
		w.Header().Set("Content-Type", "application/json")
		sku := r.PathValue("sku")
		n, err := strconv.Atoi(strings.TrimPrefix(sku, "SKU-"))
		if !strings.HasPrefix(sku, "SKU-") || err != nil || n < 1 || n > 10000 {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"not found"}`))
			return
		}
		fmt.Fprintf(w, `{"sku":"%s","name":"Product %d","price_cents":%d,"currency":"USD","stock":%d}`,
			sku, n, 100+(n*37)%9900, 500+n%500)
	})
	http.ListenAndServe(":9000", nil)
}

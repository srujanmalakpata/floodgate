// Command upstream is a tiny demo backend for docker-compose and Kubernetes
// examples. It echoes which instance served the request and the client headers
// the gateway forwarded.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := ":" + envOr("PORT", "9000")
	name, _ := os.Hostname()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"upstream":        name,
			"method":          r.Method,
			"path":            r.URL.Path,
			"x_forwarded_for": r.Header.Get("X-Forwarded-For"),
		})
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("upstream listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

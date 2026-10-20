package analytics

import (
	"context"
	"encoding/json"
	"github.com/justinrush/q/internal/api"
	"io/fs"
	"net/http"
	"strings"
)

type Source interface {
	Analytics(context.Context) (api.AnalyticsSnapshot, error)
	Refresh() (bool, error)
}

// Handler exposes only read APIs. The daemon credential stays in Source.
// Host/origin checks prevent unrelated browser origins accessing local records.
func Handler(source Source, assets fs.FS, host string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/query", func(w http.ResponseWriter, r *http.Request) {
		f, err := ParseFilter(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _ = source.Refresh()
		snapshot, err := source.Analytics(r.Context())
		if err != nil {
			http.Error(w, "q daemon unavailable; retrying will reconnect", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Query(snapshot, f))
	})
	mux.Handle("GET /", http.FileServerFS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != "http://"+host) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		mux.ServeHTTP(w, r)
	})
}

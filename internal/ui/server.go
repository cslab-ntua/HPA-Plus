package ui

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

//go:embed web
var web embed.FS

// Server serves the page and its read-only JSON API.
type Server struct {
	Reader *Reader
	// Namespace limits the overview; empty lists every namespace the caller can read.
	Namespace string
	// Hosts are the host names a request may address. Any other Host header is
	// refused, so a web page that rebinds its own name to this address cannot
	// read the cluster through the caller's credentials.
	Hosts []string
}

func (s *Server) Handler() http.Handler {
	static, err := fs.Sub(web, "web")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/api/hpapluses", s.list)
	mux.HandleFunc("/api/hpapluses/", s.detail)
	return s.guard(mux)
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		allowed := false
		for _, name := range s.Hosts {
			allowed = allowed || strings.EqualFold(host, name)
		}
		if !allowed {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	namespace := s.Namespace
	if namespace == "" {
		namespace = r.URL.Query().Get("namespace")
	}
	items, err := s.Reader.Overview(ctx, namespace)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]interface{}{"namespace": namespace, "fixedNamespace": s.Namespace != "", "items": items, "now": s.Reader.now()})
}

// detail serves /api/hpapluses/{namespace}/{name} and its /trend sparkline.
func (s *Server) detail(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/hpapluses/"), "/")
	trend := len(parts) == 3 && parts[2] == "trend"
	if (len(parts) != 2 && !trend) || parts[0] == "" || parts[1] == "" || (s.Namespace != "" && parts[0] != s.Namespace) {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if trend {
		samples, err := s.Reader.Trend(ctx, parts[0], parts[1])
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]interface{}{"samples": samples})
		return
	}
	detail, err := s.Reader.Detail(ctx, parts[0], parts[1])
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, detail)
}

func writeJSON(w http.ResponseWriter, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case apierrors.IsNotFound(err):
		status = http.StatusNotFound
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		status = http.StatusForbidden
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

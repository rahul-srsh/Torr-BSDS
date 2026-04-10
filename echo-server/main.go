package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/rahul-srsh/Torr-BSDS/shared/config"
	sharedserver "github.com/rahul-srsh/Torr-BSDS/shared/server"
)

type echoResponse struct {
	NodeType  string            `json:"nodeType"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Query     map[string]string `json:"query"`
	Headers   map[string]string `json:"headers"`
	Body      string            `json:"body"`
	BodyBytes int               `json:"bodyBytes"`
	Timestamp time.Time         `json:"timestamp"`
}

func main() {
	cfg := config.Load()
	srv := sharedserver.New(cfg)
	srv.Mux.HandleFunc("/echo", echoHandler(cfg.NodeType))
	srv.Start()
}

func echoHandler(nodeType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC()

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		query := make(map[string]string, len(r.URL.Query()))
		for key, values := range r.URL.Query() {
			if len(values) > 0 {
				query[key] = values[0]
			}
		}

		headers := make(map[string]string, len(r.Header))
		for key, values := range r.Header {
			if len(values) > 0 {
				headers[key] = values[0]
			}
		}

		log.Printf("[echo] %s %s from %s at %s", r.Method, r.URL.Path, r.RemoteAddr, now.Format(time.RFC3339Nano))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(echoResponse{
			NodeType:  nodeType,
			Method:    r.Method,
			Path:      r.URL.Path,
			Query:     query,
			Headers:   headers,
			Body:      string(body),
			BodyBytes: len(body),
			Timestamp: now,
		})
	}
}

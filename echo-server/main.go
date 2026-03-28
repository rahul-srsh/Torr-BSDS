package main

import (
	"encoding/json"
	"io"
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

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(echoResponse{
			NodeType:  nodeType,
			Method:    r.Method,
			Path:      r.URL.Path,
			Query:     query,
			Body:      string(body),
			BodyBytes: len(body),
			Timestamp: time.Now().UTC(),
		})
	}
}

// Package server provides the HTTP base used by every HopVault node: a shared
// /health endpoint and an exported mux that services extend with their own routes.
package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/rahul-srsh/Torr-BSDS/shared/config"
)

type BaseServer struct {
	Config *config.NodeConfig
	Mux    *http.ServeMux
}

func New(cfg *config.NodeConfig) *BaseServer {
	mux := http.NewServeMux()
	s := &BaseServer{
		Config: cfg,
		Mux:    mux,
	}

	mux.HandleFunc("/health", s.healthHandler)

	return s
}

func (s *BaseServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":    "ok",
		"node_type": s.Config.NodeType,
	})
}

func (s *BaseServer) Start() {
	addr := ":" + s.Config.Port
	log.Printf("[server] %s node starting on %s", s.Config.NodeType, addr)
	if err := http.ListenAndServe(addr, s.Mux); err != nil {
		log.Fatalf("[server] failed to start: %v", err)
	}
}
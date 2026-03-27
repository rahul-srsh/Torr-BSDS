package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/rahul-srsh/Torr-BSDS/shared/config"
	sharedserver "github.com/rahul-srsh/Torr-BSDS/shared/server"
)

var (
	loadConfig  = config.Load
	startServer = func(s *sharedserver.BaseServer) {
		s.Start()
	}
)

type NodeRegistration struct {
	NodeID    string `json:"nodeId"`
	NodeType  string `json:"nodeType"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	PublicKey string `json:"publicKey"`
}

type NodeRegistry struct {
	mu    sync.RWMutex
	nodes map[string]NodeRegistration
}

func NewNodeRegistry() *NodeRegistry {
	return &NodeRegistry{
		nodes: make(map[string]NodeRegistration),
	}
}

func (r *NodeRegistry) Upsert(node NodeRegistration) NodeRegistration {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nodes[node.NodeID] = node

	return node
}

func (r *NodeRegistry) Delete(nodeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.nodes[nodeID]; !ok {
		return false
	}

	delete(r.nodes, nodeID)
	return true
}

func (r *NodeRegistry) List() []NodeRegistration {
	r.mu.RLock()
	defer r.mu.RUnlock()

	nodes := make([]NodeRegistration, 0, len(r.nodes))
	for _, node := range r.nodes {
		nodes = append(nodes, node)
	}

	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].NodeID < nodes[j].NodeID
	})

	return nodes
}

type DirectoryServer struct {
	registry *NodeRegistry
}

func NewDirectoryServer(registry *NodeRegistry) *DirectoryServer {
	if registry == nil {
		registry = NewNodeRegistry()
	}

	return &DirectoryServer{registry: registry}
}

func (s *DirectoryServer) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /register", s.registerHandler)
	mux.HandleFunc("DELETE /deregister/{nodeId}", s.deregisterHandler)
	mux.HandleFunc("GET /debug/nodes", s.debugNodesHandler)
}

func (s *DirectoryServer) registerHandler(w http.ResponseWriter, r *http.Request) {
	var payload NodeRegistration
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	if decoder.More() {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	if err := validateNodeRegistration(payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, s.registry.Upsert(payload))
}

func (s *DirectoryServer) deregisterHandler(w http.ResponseWriter, r *http.Request) {
	nodeID := strings.TrimSpace(r.PathValue("nodeId"))
	if nodeID == "" {
		writeJSONError(w, http.StatusBadRequest, "nodeId is required")
		return
	}

	if !s.registry.Delete(nodeID) {
		writeJSONError(w, http.StatusNotFound, "node not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "deregistered",
		"nodeId": nodeID,
	})
}

func (s *DirectoryServer) debugNodesHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.registry.List())
}

func validateNodeRegistration(node NodeRegistration) error {
	switch {
	case strings.TrimSpace(node.NodeID) == "":
		return errors.New("nodeId is required")
	case !isUUID(node.NodeID):
		return errors.New("nodeId must be a valid UUID")
	case strings.TrimSpace(node.NodeType) == "":
		return errors.New("nodeType is required")
	case !isValidNodeType(node.NodeType):
		return errors.New("nodeType must be one of: guard, relay, exit")
	case strings.TrimSpace(node.Host) == "":
		return errors.New("host is required")
	case node.Port <= 0 || node.Port > 65535:
		return errors.New("port must be between 1 and 65535")
	case strings.TrimSpace(node.PublicKey) == "":
		return errors.New("publicKey is required")
	default:
		return nil
	}
}

func isValidNodeType(nodeType string) bool {
	switch nodeType {
	case "guard", "relay", "exit":
		return true
	default:
		return false
	}
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}

	for i, r := range value {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHexDigit(byte(r)) {
				return false
			}
		}
	}

	return true
}

func isHexDigit(r byte) bool {
	return ('0' <= r && r <= '9') || ('a' <= r && r <= 'f') || ('A' <= r && r <= 'F')
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{
		"error": message,
	})
}

func newHandler(cfg *config.NodeConfig) http.Handler {
	base := sharedserver.New(cfg)
	directoryServer := NewDirectoryServer(nil)
	directoryServer.RegisterRoutes(base.Mux)
	return base.Mux
}

func run() {
	cfg := loadConfig()
	base := sharedserver.New(cfg)
	directoryServer := NewDirectoryServer(nil)
	directoryServer.RegisterRoutes(base.Mux)
	startServer(base)
}

func main() {
	run()
}

func (n NodeRegistration) String() string {
	return fmt.Sprintf("%s:%d (%s)", n.Host, n.Port, n.NodeType)
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rahul-srsh/Torr-BSDS/shared/config"
	sharedserver "github.com/rahul-srsh/Torr-BSDS/shared/server"
)

const (
	StatusHealthy   = "HEALTHY"
	StatusUnhealthy = "UNHEALTHY"
)

var (
	loadConfig  = config.Load
	startServer = func(s *sharedserver.BaseServer) {
		s.Start()
	}
	newTicker = func(d time.Duration) ticker {
		return realTicker{Ticker: time.NewTicker(d)}
	}
)

type ticker interface {
	C() <-chan time.Time
	Stop()
}

type realTicker struct {
	*time.Ticker
}

func (t realTicker) C() <-chan time.Time {
	return t.Ticker.C
}

type logFunc func(format string, args ...any)

type NodeRegistration struct {
	NodeID    string `json:"nodeId"`
	NodeType  string `json:"nodeType"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	PublicKey string `json:"publicKey"`
}

type NodeRecord struct {
	NodeRegistration
	LastSeen time.Time `json:"lastSeen"`
	Status   string    `json:"status"`
}

type CircuitResponse struct {
	Guard NodeRecord `json:"guard"`
	Relay NodeRecord `json:"relay"`
	Exit  NodeRecord `json:"exit"`
}

type NodeRegistry struct {
	mu    sync.RWMutex
	nodes map[string]NodeRecord
}

func NewNodeRegistry() *NodeRegistry {
	return &NodeRegistry{
		nodes: make(map[string]NodeRecord),
	}
}

func (r *NodeRegistry) Upsert(node NodeRegistration, now time.Time) NodeRecord {
	r.mu.Lock()
	defer r.mu.Unlock()

	record := NodeRecord{
		NodeRegistration: node,
		LastSeen:         now.UTC(),
		Status:           StatusHealthy,
	}

	r.nodes[node.NodeID] = record
	return record
}

func (r *NodeRegistry) Heartbeat(nodeID string, now time.Time) (NodeRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	record, ok := r.nodes[nodeID]
	if !ok {
		return NodeRecord{}, false
	}

	record.LastSeen = now.UTC()
	record.Status = StatusHealthy
	r.nodes[nodeID] = record
	return record, true
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

func (r *NodeRegistry) List(includeUnhealthy bool) []NodeRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()

	nodes := make([]NodeRecord, 0, len(r.nodes))
	for _, node := range r.nodes {
		if !includeUnhealthy && node.Status != StatusHealthy {
			continue
		}
		nodes = append(nodes, node)
	}

	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].NodeID < nodes[j].NodeID
	})

	return nodes
}

func (r *NodeRegistry) BuildCircuit() (CircuitResponse, bool) {
	healthyNodes := r.List(false)
	circuit := CircuitResponse{}

	for _, node := range healthyNodes {
		switch node.NodeType {
		case "guard":
			if circuit.Guard.NodeID == "" {
				circuit.Guard = node
			}
		case "relay":
			if circuit.Relay.NodeID == "" {
				circuit.Relay = node
			}
		case "exit":
			if circuit.Exit.NodeID == "" {
				circuit.Exit = node
			}
		}
	}

	return circuit, circuit.Guard.NodeID != "" && circuit.Relay.NodeID != "" && circuit.Exit.NodeID != ""
}

func (r *NodeRegistry) MarkUnhealthy(now time.Time, timeout time.Duration) []NodeRecord {
	r.mu.Lock()
	defer r.mu.Unlock()

	transitions := make([]NodeRecord, 0)
	for nodeID, node := range r.nodes {
		if node.Status == StatusUnhealthy {
			continue
		}
		if now.Sub(node.LastSeen) <= timeout {
			continue
		}

		node.Status = StatusUnhealthy
		r.nodes[nodeID] = node
		transitions = append(transitions, node)
	}

	sort.Slice(transitions, func(i, j int) bool {
		return transitions[i].NodeID < transitions[j].NodeID
	})

	return transitions
}

type DirectoryServer struct {
	registry         *NodeRegistry
	now              func() time.Time
	logf             logFunc
	cleanupInterval  time.Duration
	heartbeatTimeout time.Duration
}

func NewDirectoryServer(registry *NodeRegistry, cleanupInterval, heartbeatTimeout time.Duration) *DirectoryServer {
	if registry == nil {
		registry = NewNodeRegistry()
	}

	return &DirectoryServer{
		registry:         registry,
		now:              time.Now,
		logf:             log.Printf,
		cleanupInterval:  cleanupInterval,
		heartbeatTimeout: heartbeatTimeout,
	}
}

func (s *DirectoryServer) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /register", s.registerHandler)
	mux.HandleFunc("POST /heartbeat/{nodeId}", s.heartbeatHandler)
	mux.HandleFunc("DELETE /deregister/{nodeId}", s.deregisterHandler)
	mux.HandleFunc("GET /nodes", s.nodesHandler)
	mux.HandleFunc("GET /circuit", s.circuitHandler)
	mux.HandleFunc("GET /debug/nodes", s.debugNodesHandler)
}

func (s *DirectoryServer) StartHealthMonitor(ctx context.Context) {
	t := newTicker(s.cleanupInterval)
	defer t.Stop()
	s.runHealthMonitor(ctx, t.C())
}

func (s *DirectoryServer) runHealthMonitor(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case tickTime, ok := <-ticks:
			if !ok {
				return
			}
			s.cleanupUnhealthyNodes(tickTime)
		}
	}
}

func (s *DirectoryServer) cleanupUnhealthyNodes(now time.Time) {
	for _, node := range s.registry.MarkUnhealthy(now.UTC(), s.heartbeatTimeout) {
		s.logf("[directory-server] node %s transitioned to %s", node.NodeID, StatusUnhealthy)
	}
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

	writeJSON(w, http.StatusCreated, s.registry.Upsert(payload, s.now()))
}

func (s *DirectoryServer) heartbeatHandler(w http.ResponseWriter, r *http.Request) {
	nodeID := strings.TrimSpace(r.PathValue("nodeId"))
	if nodeID == "" {
		writeJSONError(w, http.StatusBadRequest, "nodeId is required")
		return
	}

	record, ok := s.registry.Heartbeat(nodeID, s.now())
	if !ok {
		writeJSONError(w, http.StatusNotFound, "node not found")
		return
	}

	writeJSON(w, http.StatusOK, record)
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

func (s *DirectoryServer) nodesHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.registry.List(false))
}

func (s *DirectoryServer) circuitHandler(w http.ResponseWriter, _ *http.Request) {
	circuit, ok := s.registry.BuildCircuit()
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "not enough healthy nodes to build a circuit")
		return
	}

	writeJSON(w, http.StatusOK, circuit)
}

func (s *DirectoryServer) debugNodesHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.registry.List(true))
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
	directoryServer := NewDirectoryServer(nil, cfg.HeartbeatCleanupInterval, cfg.HeartbeatTimeout)
	directoryServer.RegisterRoutes(base.Mux)
	return base.Mux
}

func run() {
	cfg := loadConfig()
	base := sharedserver.New(cfg)
	directoryServer := NewDirectoryServer(nil, cfg.HeartbeatCleanupInterval, cfg.HeartbeatTimeout)
	directoryServer.RegisterRoutes(base.Mux)
	go directoryServer.StartHealthMonitor(context.Background())
	startServer(base)
}

func main() {
	run()
}

func (n NodeRegistration) String() string {
	return fmt.Sprintf("%s:%d (%s)", n.Host, n.Port, n.NodeType)
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/rahul-srsh/Torr-BSDS/shared/config"
	sharedserver "github.com/rahul-srsh/Torr-BSDS/shared/server"
)

func TestRegisterHeartbeatNodesAndCircuitHandlers(t *testing.T) {
	now := time.Date(2026, time.March, 26, 12, 0, 0, 0, time.UTC)
	server := newTestDirectoryServer(now, 15*time.Second, 30*time.Second)
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	server.randomIntn = func(int) int { return 0 }

	guard := NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174000",
		NodeType:  "guard",
		Host:      "10.0.0.10",
		Port:      9001,
		PublicKey: "pubkey-1",
	}
	relay := NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174001",
		NodeType:  "relay",
		Host:      "10.0.0.11",
		Port:      9002,
		PublicKey: "pubkey-2",
	}
	exit := NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174002",
		NodeType:  "exit",
		Host:      "10.0.0.12",
		Port:      9003,
		PublicKey: "pubkey-3",
	}

	for _, node := range []NodeRegistration{guard, relay, exit} {
		recorder := performJSONRequest(t, mux, http.MethodPost, "/register", node)
		assertStatus(t, recorder, http.StatusCreated)

		var created NodeRecord
		decodeResponse(t, recorder, &created)
		if created.NodeRegistration != node {
			t.Fatalf("created registration = %+v, want %+v", created.NodeRegistration, node)
		}
		if created.Status != StatusHealthy {
			t.Fatalf("status = %q, want %q", created.Status, StatusHealthy)
		}
		if !created.LastSeen.Equal(now) {
			t.Fatalf("lastSeen = %s, want %s", created.LastSeen, now)
		}
	}

	updatedRelay := relay
	updatedRelay.Host = "relay.hopvault.local"
	server.now = func() time.Time { return now.Add(5 * time.Second) }
	recorder := performJSONRequest(t, mux, http.MethodPost, "/register", updatedRelay)
	assertStatus(t, recorder, http.StatusCreated)

	var updatedRecord NodeRecord
	decodeResponse(t, recorder, &updatedRecord)
	if updatedRecord.NodeRegistration != updatedRelay {
		t.Fatalf("updated relay = %+v, want %+v", updatedRecord.NodeRegistration, updatedRelay)
	}

	server.now = func() time.Time { return now.Add(10 * time.Second) }
	heartbeatRecorder := httptest.NewRecorder()
	heartbeatRequest := httptest.NewRequest(http.MethodPost, "/heartbeat/"+guard.NodeID, nil)
	mux.ServeHTTP(heartbeatRecorder, heartbeatRequest)

	assertStatus(t, heartbeatRecorder, http.StatusOK)

	var heartbeatRecord NodeRecord
	decodeResponse(t, heartbeatRecorder, &heartbeatRecord)
	if heartbeatRecord.NodeID != guard.NodeID {
		t.Fatalf("heartbeat nodeId = %q, want %q", heartbeatRecord.NodeID, guard.NodeID)
	}
	if !heartbeatRecord.LastSeen.Equal(now.Add(10 * time.Second)) {
		t.Fatalf("heartbeat lastSeen = %s, want %s", heartbeatRecord.LastSeen, now.Add(10*time.Second))
	}

	nodesRecorder := httptest.NewRecorder()
	nodesRequest := httptest.NewRequest(http.MethodGet, "/nodes", nil)
	mux.ServeHTTP(nodesRecorder, nodesRequest)

	assertStatus(t, nodesRecorder, http.StatusOK)

	var healthyNodes NodesByTypeResponse
	decodeResponse(t, nodesRecorder, &healthyNodes)
	if len(healthyNodes.Guard) != 1 || len(healthyNodes.Relay) != 1 || len(healthyNodes.Exit) != 1 {
		t.Fatalf("grouped healthy nodes = %+v, want 1 guard, 1 relay, 1 exit", healthyNodes)
	}

	circuitRecorder := httptest.NewRecorder()
	circuitRequest := httptest.NewRequest(http.MethodGet, "/circuit", nil)
	mux.ServeHTTP(circuitRecorder, circuitRequest)

	assertStatus(t, circuitRecorder, http.StatusOK)

	var circuit CircuitResponse
	decodeResponse(t, circuitRecorder, &circuit)
	if circuit.Guard.NodeType != "guard" || circuit.Relay.NodeType != "relay" || circuit.Exit.NodeType != "exit" {
		t.Fatalf("unexpected circuit = %+v", circuit)
	}

	singleHopRecorder := httptest.NewRecorder()
	singleHopRequest := httptest.NewRequest(http.MethodGet, "/circuit?hops=1", nil)
	mux.ServeHTTP(singleHopRecorder, singleHopRequest)
	assertStatus(t, singleHopRecorder, http.StatusOK)

	var singleHop CircuitResponse
	decodeResponse(t, singleHopRecorder, &singleHop)
	if singleHop.Guard.NodeType != "guard" {
		t.Fatalf("single hop guard = %+v, want guard node", singleHop.Guard)
	}
	if singleHop.Relay.NodeID != "" || singleHop.Exit.NodeID != "" {
		t.Fatalf("single hop circuit should only include a guard: %+v", singleHop)
	}

	debugRecorder := httptest.NewRecorder()
	debugRequest := httptest.NewRequest(http.MethodGet, "/debug/nodes", nil)
	mux.ServeHTTP(debugRecorder, debugRequest)

	assertStatus(t, debugRecorder, http.StatusOK)

	var debugNodes []NodeRecord
	decodeResponse(t, debugRecorder, &debugNodes)
	if len(debugNodes) != 3 {
		t.Fatalf("len(debugNodes) = %d, want 3", len(debugNodes))
	}
}

func TestHandlersRejectInvalidRequests(t *testing.T) {
	server := newTestDirectoryServer(time.Date(2026, time.March, 26, 12, 0, 0, 0, time.UTC), 15*time.Second, 30*time.Second)
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)

	tests := []struct {
		name           string
		method         string
		path           string
		body           any
		rawBody        string
		expectedStatus int
		expectedError  string
	}{
		{
			name:           "invalid json",
			method:         http.MethodPost,
			path:           "/register",
			rawBody:        `{"nodeId":`,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "invalid JSON payload",
		},
		{
			name:           "multiple json values",
			method:         http.MethodPost,
			path:           "/register",
			rawBody:        `{"nodeId":"123e4567-e89b-12d3-a456-426614174011","nodeType":"guard","host":"127.0.0.1","port":8080,"publicKey":"pub"}{"extra":true}`,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "invalid JSON payload",
		},
		{
			name:   "missing node id",
			method: http.MethodPost,
			path:   "/register",
			body: map[string]any{
				"nodeType":  "guard",
				"host":      "127.0.0.1",
				"port":      8080,
				"publicKey": "pub",
			},
			expectedStatus: http.StatusBadRequest,
			expectedError:  "nodeId is required",
		},
		{
			name:   "invalid uuid",
			method: http.MethodPost,
			path:   "/register",
			body: map[string]any{
				"nodeId":    "not-a-uuid",
				"nodeType":  "guard",
				"host":      "127.0.0.1",
				"port":      8080,
				"publicKey": "pub",
			},
			expectedStatus: http.StatusBadRequest,
			expectedError:  "nodeId must be a valid UUID",
		},
		{
			name:   "missing node type",
			method: http.MethodPost,
			path:   "/register",
			body: map[string]any{
				"nodeId":    "123e4567-e89b-12d3-a456-426614174010",
				"host":      "127.0.0.1",
				"port":      8080,
				"publicKey": "pub",
			},
			expectedStatus: http.StatusBadRequest,
			expectedError:  "nodeType is required",
		},
		{
			name:   "invalid node type",
			method: http.MethodPost,
			path:   "/register",
			body: map[string]any{
				"nodeId":    "123e4567-e89b-12d3-a456-426614174012",
				"nodeType":  "middle",
				"host":      "127.0.0.1",
				"port":      8080,
				"publicKey": "pub",
			},
			expectedStatus: http.StatusBadRequest,
			expectedError:  "nodeType must be one of: guard, relay, exit",
		},
		{
			name:   "missing host",
			method: http.MethodPost,
			path:   "/register",
			body: map[string]any{
				"nodeId":    "123e4567-e89b-12d3-a456-426614174013",
				"nodeType":  "relay",
				"host":      "",
				"port":      8080,
				"publicKey": "pub",
			},
			expectedStatus: http.StatusBadRequest,
			expectedError:  "host is required",
		},
		{
			name:   "invalid port",
			method: http.MethodPost,
			path:   "/register",
			body: map[string]any{
				"nodeId":    "123e4567-e89b-12d3-a456-426614174014",
				"nodeType":  "exit",
				"host":      "127.0.0.1",
				"port":      70000,
				"publicKey": "pub",
			},
			expectedStatus: http.StatusBadRequest,
			expectedError:  "port must be between 1 and 65535",
		},
		{
			name:   "missing public key",
			method: http.MethodPost,
			path:   "/register",
			body: map[string]any{
				"nodeId":    "123e4567-e89b-12d3-a456-426614174015",
				"nodeType":  "guard",
				"host":      "127.0.0.1",
				"port":      8080,
				"publicKey": "",
			},
			expectedStatus: http.StatusBadRequest,
			expectedError:  "publicKey is required",
		},
		{
			name:           "unknown heartbeat",
			method:         http.MethodPost,
			path:           "/heartbeat/123e4567-e89b-12d3-a456-426614174016",
			expectedStatus: http.StatusNotFound,
			expectedError:  "node not found",
		},
		{
			name:           "unknown deregister",
			method:         http.MethodDelete,
			path:           "/deregister/123e4567-e89b-12d3-a456-426614174017",
			expectedStatus: http.StatusNotFound,
			expectedError:  "node not found",
		},
		{
			name:           "circuit unavailable",
			method:         http.MethodGet,
			path:           "/circuit",
			expectedStatus: http.StatusServiceUnavailable,
			expectedError:  "not enough healthy nodes to build a circuit",
		},
		{
			name:           "invalid hops parameter",
			method:         http.MethodGet,
			path:           "/circuit?hops=2",
			expectedStatus: http.StatusBadRequest,
			expectedError:  "hops must be 1 or 3",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := newRequest(t, tc.method, tc.path, tc.body, tc.rawBody)
			mux.ServeHTTP(recorder, req)

			assertStatus(t, recorder, tc.expectedStatus)
			var response map[string]string
			decodeResponse(t, recorder, &response)
			if response["error"] != tc.expectedError {
				t.Fatalf("error = %q, want %q", response["error"], tc.expectedError)
			}
		})
	}

	missingIDHeartbeatRecorder := httptest.NewRecorder()
	missingIDHeartbeatRequest := httptest.NewRequest(http.MethodPost, "/heartbeat/", nil)
	missingIDHeartbeatRequest.SetPathValue("nodeId", "")
	server.heartbeatHandler(missingIDHeartbeatRecorder, missingIDHeartbeatRequest)
	assertStatus(t, missingIDHeartbeatRecorder, http.StatusBadRequest)

	var heartbeatResponse map[string]string
	decodeResponse(t, missingIDHeartbeatRecorder, &heartbeatResponse)
	if heartbeatResponse["error"] != "nodeId is required" {
		t.Fatalf("error = %q, want %q", heartbeatResponse["error"], "nodeId is required")
	}

	missingIDDeregisterRecorder := httptest.NewRecorder()
	missingIDDeregisterRequest := httptest.NewRequest(http.MethodDelete, "/deregister/", nil)
	missingIDDeregisterRequest.SetPathValue("nodeId", "")
	server.deregisterHandler(missingIDDeregisterRecorder, missingIDDeregisterRequest)
	assertStatus(t, missingIDDeregisterRecorder, http.StatusBadRequest)

	var deregisterResponse map[string]string
	decodeResponse(t, missingIDDeregisterRecorder, &deregisterResponse)
	if deregisterResponse["error"] != "nodeId is required" {
		t.Fatalf("error = %q, want %q", deregisterResponse["error"], "nodeId is required")
	}
}

func TestCircuitRandomizationAndGroupedNodes(t *testing.T) {
	baseTime := time.Date(2026, time.March, 27, 9, 0, 0, 0, time.UTC)
	server := newTestDirectoryServer(baseTime, 15*time.Second, 30*time.Second)
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)

	sequence := []int{0, 0, 0, 1, 1, 1}
	server.randomIntn = func(int) int {
		next := sequence[0]
		sequence = sequence[1:]
		return next
	}

	nodes := []NodeRegistration{
		{NodeID: "123e4567-e89b-12d3-a456-426614174050", NodeType: "guard", Host: "guard-1.local", Port: 9101, PublicKey: "guard-key-1"},
		{NodeID: "123e4567-e89b-12d3-a456-426614174051", NodeType: "guard", Host: "guard-2.local", Port: 9102, PublicKey: "guard-key-2"},
		{NodeID: "123e4567-e89b-12d3-a456-426614174052", NodeType: "relay", Host: "relay-1.local", Port: 9201, PublicKey: "relay-key-1"},
		{NodeID: "123e4567-e89b-12d3-a456-426614174053", NodeType: "relay", Host: "relay-2.local", Port: 9202, PublicKey: "relay-key-2"},
		{NodeID: "123e4567-e89b-12d3-a456-426614174054", NodeType: "exit", Host: "exit-1.local", Port: 9301, PublicKey: "exit-key-1"},
		{NodeID: "123e4567-e89b-12d3-a456-426614174055", NodeType: "exit", Host: "exit-2.local", Port: 9302, PublicKey: "exit-key-2"},
	}

	for _, node := range nodes {
		performJSONRequest(t, mux, http.MethodPost, "/register", node)
	}

	nodesRecorder := httptest.NewRecorder()
	mux.ServeHTTP(nodesRecorder, httptest.NewRequest(http.MethodGet, "/nodes", nil))
	assertStatus(t, nodesRecorder, http.StatusOK)

	var grouped NodesByTypeResponse
	decodeResponse(t, nodesRecorder, &grouped)
	if len(grouped.Guard) != 2 || len(grouped.Relay) != 2 || len(grouped.Exit) != 2 {
		t.Fatalf("grouped nodes = %+v, want 2 guard, 2 relay, 2 exit", grouped)
	}

	firstCircuitRecorder := httptest.NewRecorder()
	mux.ServeHTTP(firstCircuitRecorder, httptest.NewRequest(http.MethodGet, "/circuit", nil))
	assertStatus(t, firstCircuitRecorder, http.StatusOK)

	var firstCircuit CircuitResponse
	decodeResponse(t, firstCircuitRecorder, &firstCircuit)

	secondCircuitRecorder := httptest.NewRecorder()
	mux.ServeHTTP(secondCircuitRecorder, httptest.NewRequest(http.MethodGet, "/circuit", nil))
	assertStatus(t, secondCircuitRecorder, http.StatusOK)

	var secondCircuit CircuitResponse
	decodeResponse(t, secondCircuitRecorder, &secondCircuit)

	if firstCircuit.Guard.NodeID == secondCircuit.Guard.NodeID &&
		firstCircuit.Relay.NodeID == secondCircuit.Relay.NodeID &&
		firstCircuit.Exit.NodeID == secondCircuit.Exit.NodeID {
		t.Fatalf("expected different circuit combinations, got %+v and %+v", firstCircuit, secondCircuit)
	}

	if firstCircuit.Guard.Host == "" || firstCircuit.Guard.Port == 0 || firstCircuit.Guard.PublicKey == "" {
		t.Fatalf("first circuit guard missing connection fields: %+v", firstCircuit.Guard)
	}
	if secondCircuit.Exit.Host == "" || secondCircuit.Exit.Port == 0 || secondCircuit.Exit.PublicKey == "" {
		t.Fatalf("second circuit exit missing connection fields: %+v", secondCircuit.Exit)
	}
}

func TestHealthMonitorTransitionsAndFiltersNodes(t *testing.T) {
	baseTime := time.Date(2026, time.March, 26, 12, 0, 0, 0, time.UTC)
	server := newTestDirectoryServer(baseTime, 2*time.Second, 30*time.Second)
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)

	logs := make([]string, 0)
	server.logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	nodes := []NodeRegistration{
		{
			NodeID:    "123e4567-e89b-12d3-a456-426614174020",
			NodeType:  "guard",
			Host:      "guard.local",
			Port:      9001,
			PublicKey: "guard-key",
		},
		{
			NodeID:    "123e4567-e89b-12d3-a456-426614174021",
			NodeType:  "relay",
			Host:      "relay.local",
			Port:      9002,
			PublicKey: "relay-key",
		},
		{
			NodeID:    "123e4567-e89b-12d3-a456-426614174022",
			NodeType:  "exit",
			Host:      "exit.local",
			Port:      9003,
			PublicKey: "exit-key",
		},
	}

	for _, node := range nodes {
		performJSONRequest(t, mux, http.MethodPost, "/register", node)
	}

	server.now = func() time.Time { return baseTime.Add(35 * time.Second) }
	server.cleanupUnhealthyNodes(server.now())

	if len(logs) != 3 {
		t.Fatalf("len(logs) = %d, want 3", len(logs))
	}

	debugRecorder := httptest.NewRecorder()
	debugRequest := httptest.NewRequest(http.MethodGet, "/debug/nodes", nil)
	mux.ServeHTTP(debugRecorder, debugRequest)
	assertStatus(t, debugRecorder, http.StatusOK)

	var allNodes []NodeRecord
	decodeResponse(t, debugRecorder, &allNodes)
	for _, node := range allNodes {
		if node.Status != StatusUnhealthy {
			t.Fatalf("status = %q, want %q", node.Status, StatusUnhealthy)
		}
	}

	nodesRecorder := httptest.NewRecorder()
	nodesRequest := httptest.NewRequest(http.MethodGet, "/nodes", nil)
	mux.ServeHTTP(nodesRecorder, nodesRequest)
	assertStatus(t, nodesRecorder, http.StatusOK)

	var healthyNodes NodesByTypeResponse
	decodeResponse(t, nodesRecorder, &healthyNodes)
	if len(healthyNodes.Guard) != 0 || len(healthyNodes.Relay) != 0 || len(healthyNodes.Exit) != 0 {
		t.Fatalf("grouped healthy nodes = %+v, want all groups empty", healthyNodes)
	}

	circuitRecorder := httptest.NewRecorder()
	circuitRequest := httptest.NewRequest(http.MethodGet, "/circuit", nil)
	mux.ServeHTTP(circuitRecorder, circuitRequest)
	assertStatus(t, circuitRecorder, http.StatusServiceUnavailable)

	server.now = func() time.Time { return baseTime.Add(40 * time.Second) }
	heartbeatRecorder := httptest.NewRecorder()
	heartbeatRequest := httptest.NewRequest(http.MethodPost, "/heartbeat/"+nodes[0].NodeID, nil)
	mux.ServeHTTP(heartbeatRecorder, heartbeatRequest)
	assertStatus(t, heartbeatRecorder, http.StatusOK)

	nodesRecorder = httptest.NewRecorder()
	mux.ServeHTTP(nodesRecorder, httptest.NewRequest(http.MethodGet, "/nodes", nil))
	assertStatus(t, nodesRecorder, http.StatusOK)
	decodeResponse(t, nodesRecorder, &healthyNodes)
	if len(healthyNodes.Guard) != 1 || len(healthyNodes.Relay) != 0 || len(healthyNodes.Exit) != 0 {
		t.Fatalf("grouped healthy nodes = %+v, want only one healthy guard", healthyNodes)
	}
	if healthyNodes.Guard[0].NodeID != nodes[0].NodeID {
		t.Fatalf("nodeId = %q, want %q", healthyNodes.Guard[0].NodeID, nodes[0].NodeID)
	}
}

func TestHealthMonitorLoopAndTicker(t *testing.T) {
	baseTime := time.Date(2026, time.March, 26, 12, 0, 0, 0, time.UTC)
	server := newTestDirectoryServer(baseTime, 5*time.Second, 30*time.Second)
	server.registry.Upsert(NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174030",
		NodeType:  "guard",
		Host:      "guard.local",
		Port:      9001,
		PublicKey: "guard-key",
	}, baseTime)

	logs := make([]string, 0)
	server.logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	done := make(chan struct{})
	ticks := make(chan time.Time)

	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		server.runHealthMonitor(ctx, ticks)
		close(done)
	}()

	<-done

	originalTicker := newTicker
	defer func() {
		newTicker = originalTicker
	}()

	fake := &fakeTicker{ch: make(chan time.Time, 1)}
	newTicker = func(d time.Duration) ticker {
		if d != 5*time.Second {
			t.Fatalf("ticker duration = %s, want %s", d, 5*time.Second)
		}
		return fake
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	monitorDone := make(chan struct{})

	go func() {
		server.StartHealthMonitor(ctx)
		close(monitorDone)
	}()

	tickTime := baseTime.Add(31 * time.Second)
	fake.ch <- tickTime
	close(fake.ch)
	<-monitorDone

	if !fake.stopped {
		t.Fatal("expected fake ticker to be stopped")
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(logs))
	}
	if logs[0] != "[directory-server] node 123e4567-e89b-12d3-a456-426614174030 transitioned to UNHEALTHY" {
		t.Fatalf("log = %q, want %q", logs[0], "[directory-server] node 123e4567-e89b-12d3-a456-426614174030 transitioned to UNHEALTHY")
	}

	real := realTicker{Ticker: time.NewTicker(time.Second)}
	defer real.Stop()
	if real.C() == nil {
		t.Fatal("expected real ticker channel to be non-nil")
	}
}

func TestNodeRegistryAndHelpers(t *testing.T) {
	registry := NewNodeRegistry()
	const goroutineCount = 16
	now := time.Date(2026, time.March, 26, 12, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	wg.Add(goroutineCount)

	for i := range goroutineCount {
		go func(i int) {
			defer wg.Done()
			registry.Upsert(NodeRegistration{
				NodeID:    "123e4567-e89b-12d3-a456-426614174100",
				NodeType:  "guard",
				Host:      "127.0.0.1",
				Port:      9000 + i,
				PublicKey: "pub",
			}, now)
		}(i)
	}

	wg.Wait()

	record, ok := registry.Heartbeat("123e4567-e89b-12d3-a456-426614174100", now.Add(5*time.Second))
	if !ok {
		t.Fatal("expected heartbeat to succeed")
	}
	if record.Status != StatusHealthy {
		t.Fatalf("status = %q, want %q", record.Status, StatusHealthy)
	}

	if _, ok := registry.Heartbeat("123e4567-e89b-12d3-a456-426614174101", now); ok {
		t.Fatal("expected heartbeat for missing node to fail")
	}

	if !registry.Delete("123e4567-e89b-12d3-a456-426614174100") {
		t.Fatal("expected delete to succeed")
	}
	if registry.Delete("123e4567-e89b-12d3-a456-426614174100") {
		t.Fatal("expected second delete to fail")
	}

	registry.Upsert(NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174102",
		NodeType:  "guard",
		Host:      "127.0.0.1",
		Port:      9102,
		PublicKey: "pub-2",
	}, now)
	registry.Upsert(NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174103",
		NodeType:  "relay",
		Host:      "127.0.0.1",
		Port:      9103,
		PublicKey: "pub-3",
	}, now)

	registry.nodes["123e4567-e89b-12d3-a456-426614174103"] = NodeRecord{
		NodeRegistration: NodeRegistration{
			NodeID:    "123e4567-e89b-12d3-a456-426614174103",
			NodeType:  "relay",
			Host:      "127.0.0.1",
			Port:      9103,
			PublicKey: "pub-3",
		},
		LastSeen: now.Add(-time.Minute),
		Status:   StatusUnhealthy,
	}

	transitions := registry.MarkUnhealthy(now.Add(10*time.Second), 30*time.Second)
	if len(transitions) != 0 {
		t.Fatalf("len(transitions) = %d, want 0", len(transitions))
	}

	registry.nodes["123e4567-e89b-12d3-a456-426614174102"] = NodeRecord{
		NodeRegistration: NodeRegistration{
			NodeID:    "123e4567-e89b-12d3-a456-426614174102",
			NodeType:  "guard",
			Host:      "127.0.0.1",
			Port:      9102,
			PublicKey: "pub-2",
		},
		LastSeen: now.Add(-time.Minute),
		Status:   StatusHealthy,
	}

	transitions = registry.MarkUnhealthy(now, 30*time.Second)
	if len(transitions) != 1 {
		t.Fatalf("len(transitions) = %d, want 1", len(transitions))
	}
	if transitions[0].NodeID != "123e4567-e89b-12d3-a456-426614174102" {
		t.Fatalf("transition nodeId = %q, want %q", transitions[0].NodeID, "123e4567-e89b-12d3-a456-426614174102")
	}

	groupedHealthy := registry.GroupByType(false)
	if !reflect.DeepEqual(groupedHealthy, NodesByTypeResponse{
		Guard: []NodeRecord{},
		Relay: []NodeRecord{},
		Exit:  []NodeRecord{},
	}) {
		t.Fatalf("groupedHealthy = %+v, want no healthy nodes", groupedHealthy)
	}

	groupedAll := registry.GroupByType(true)
	if len(groupedAll.Guard) != 1 || len(groupedAll.Relay) != 1 || len(groupedAll.Exit) != 0 {
		t.Fatalf("groupedAll = %+v, want one guard and one relay", groupedAll)
	}

	if isValidNodeType("guard") != true || isValidNodeType("relay") != true || isValidNodeType("exit") != true {
		t.Fatal("expected valid node types to return true")
	}
	if isValidNodeType("directory") {
		t.Fatal("unexpected valid node type")
	}

	if !isUUID("123e4567-e89b-12d3-a456-426614174999") {
		t.Fatal("expected UUID to be valid")
	}
	if isUUID("bad-uuid") {
		t.Fatal("expected UUID to be invalid")
	}
	if isUUID("123e4567xe89b-12d3-a456-426614174999") {
		t.Fatal("expected UUID with invalid separators to be invalid")
	}
	if isUUID("123e4567-e89b-12d3-a456-42661417499z") {
		t.Fatal("expected UUID with invalid hex digit to be invalid")
	}

	if !isHexDigit('a') || !isHexDigit('F') || !isHexDigit('4') {
		t.Fatal("expected hex digits to be valid")
	}
	if isHexDigit('z') {
		t.Fatal("expected non-hex digit to be invalid")
	}

	if got := (NodeRegistration{
		NodeType: "guard",
		Host:     "127.0.0.1",
		Port:     8080,
	}).String(); got != "127.0.0.1:8080 (guard)" {
		t.Fatalf("String() = %q, want %q", got, "127.0.0.1:8080 (guard)")
	}
}

func TestNewHandlerAndTrimmedDeregister(t *testing.T) {
	handler := newHandler(&config.NodeConfig{
		Port:                     "8080",
		NodeType:                 "directory-server",
		HeartbeatCleanupInterval: 15 * time.Second,
		HeartbeatTimeout:         30 * time.Second,
	})

	node := NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174200",
		NodeType:  "guard",
		Host:      "127.0.0.1",
		Port:      8080,
		PublicKey: "pub",
	}

	performJSONRequest(t, handler, http.MethodPost, "/register", node)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/deregister/"+node.NodeID, nil)
	req.SetPathValue("nodeId", "  "+node.NodeID+"  ")

	server := newTestDirectoryServer(time.Now(), 15*time.Second, 30*time.Second)
	server.registry.Upsert(node, time.Now())
	server.deregisterHandler(recorder, req)
	assertStatus(t, recorder, http.StatusOK)
}

func TestRunAndMain(t *testing.T) {
	cfg := &config.NodeConfig{
		Port:                     "9090",
		NodeType:                 "directory-server",
		HeartbeatCleanupInterval: 3 * time.Second,
		HeartbeatTimeout:         9 * time.Second,
	}

	originalLoadConfig := loadConfig
	originalStartServer := startServer
	originalTicker := newTicker
	defer func() {
		loadConfig = originalLoadConfig
		startServer = originalStartServer
		newTicker = originalTicker
	}()

	loadCalled := false
	startCalled := false
	fake := &fakeTicker{ch: make(chan time.Time, 1)}

	loadConfig = func() *config.NodeConfig {
		loadCalled = true
		return cfg
	}

	newTicker = func(d time.Duration) ticker {
		if d != cfg.HeartbeatCleanupInterval {
			t.Fatalf("ticker duration = %s, want %s", d, cfg.HeartbeatCleanupInterval)
		}
		return fake
	}

	startServer = func(s *sharedserver.BaseServer) {
		startCalled = true

		healthRecorder := httptest.NewRecorder()
		s.Mux.ServeHTTP(healthRecorder, httptest.NewRequest(http.MethodGet, "/health", nil))
		assertStatus(t, healthRecorder, http.StatusOK)

		nodesRecorder := httptest.NewRecorder()
		s.Mux.ServeHTTP(nodesRecorder, httptest.NewRequest(http.MethodGet, "/nodes", nil))
		assertStatus(t, nodesRecorder, http.StatusOK)

		fake.ch <- time.Date(2026, time.March, 26, 12, 0, 0, 0, time.UTC)
		close(fake.ch)
	}

	run()
	if !loadCalled || !startCalled {
		t.Fatalf("run() loadCalled=%t startCalled=%t, want true/true", loadCalled, startCalled)
	}

	loadCalled = false
	startCalled = false
	fake = &fakeTicker{ch: make(chan time.Time, 1)}
	newTicker = func(time.Duration) ticker { return fake }
	startServer = func(s *sharedserver.BaseServer) {
		startCalled = true
		close(fake.ch)
	}

	main()
	if !loadCalled || !startCalled {
		t.Fatalf("main() loadCalled=%t startCalled=%t, want true/true", loadCalled, startCalled)
	}
}

func newTestDirectoryServer(now time.Time, cleanupInterval, heartbeatTimeout time.Duration) *DirectoryServer {
	server := NewDirectoryServer(nil, cleanupInterval, heartbeatTimeout)
	server.now = func() time.Time { return now }
	server.logf = func(string, ...any) {}
	return server
}

type fakeTicker struct {
	ch      chan time.Time
	stopped bool
}

func (t *fakeTicker) C() <-chan time.Time {
	return t.ch
}

func (t *fakeTicker) Stop() {
	t.stopped = true
}

func performJSONRequest(t *testing.T, handler http.Handler, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()

	recorder := httptest.NewRecorder()
	req := newRequest(t, method, path, payload, "")
	handler.ServeHTTP(recorder, req)

	return recorder
}

func newRequest(t *testing.T, method, path string, payload any, rawBody string) *http.Request {
	t.Helper()

	if rawBody != "" || payload == nil {
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(rawBody)))
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func decodeResponse(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()

	if err := json.Unmarshal(recorder.Body.Bytes(), target); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; body=%s", err, recorder.Body.String())
	}
}

func assertStatus(t *testing.T, recorder *httptest.ResponseRecorder, want int) {
	t.Helper()

	if recorder.Code != want {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, want, recorder.Body.String())
	}
}

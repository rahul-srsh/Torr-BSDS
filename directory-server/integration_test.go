package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestIntegrationRegisterHeartbeatListAndCircuitFlow(t *testing.T) {
	baseTime := time.Date(2026, time.March, 27, 12, 0, 0, 0, time.UTC)
	server := newTestDirectoryServer(baseTime, 15*time.Second, 30*time.Second)
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	server.randomIntn = func(int) int { return 0 }

	guard := NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174300",
		NodeType:  "guard",
		Host:      "guard-1.local",
		Port:      9001,
		PublicKey: "guard-key-1",
	}
	relay := NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174301",
		NodeType:  "relay",
		Host:      "relay-1.local",
		Port:      9002,
		PublicKey: "relay-key-1",
	}
	exit := NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174302",
		NodeType:  "exit",
		Host:      "exit-1.local",
		Port:      9003,
		PublicKey: "exit-key-1",
	}

	for _, node := range []NodeRegistration{guard, relay, exit} {
		recorder := performJSONRequest(t, mux, http.MethodPost, "/register", node)
		assertStatus(t, recorder, http.StatusCreated)
	}

	server.now = func() time.Time { return baseTime.Add(10 * time.Second) }
	heartbeatRecorder := httptest.NewRecorder()
	mux.ServeHTTP(heartbeatRecorder, httptest.NewRequest(http.MethodPost, "/heartbeat/"+relay.NodeID, nil))
	assertStatus(t, heartbeatRecorder, http.StatusOK)

	nodesRecorder := httptest.NewRecorder()
	mux.ServeHTTP(nodesRecorder, httptest.NewRequest(http.MethodGet, "/nodes", nil))
	assertStatus(t, nodesRecorder, http.StatusOK)

	var grouped NodesByTypeResponse
	decodeResponse(t, nodesRecorder, &grouped)
	if len(grouped.Guard) != 1 || len(grouped.Relay) != 1 || len(grouped.Exit) != 1 {
		t.Fatalf("grouped nodes = %+v, want one healthy node of each type", grouped)
	}

	circuitRecorder := httptest.NewRecorder()
	mux.ServeHTTP(circuitRecorder, httptest.NewRequest(http.MethodGet, "/circuit", nil))
	assertStatus(t, circuitRecorder, http.StatusOK)

	var circuit CircuitResponse
	decodeResponse(t, circuitRecorder, &circuit)
	if circuit.Guard.NodeID != guard.NodeID || circuit.Relay.NodeID != relay.NodeID || circuit.Exit.NodeID != exit.NodeID {
		t.Fatalf("circuit = %+v, want registered guard/relay/exit", circuit)
	}
	if circuit.Guard.Host == "" || circuit.Relay.Port == 0 || circuit.Exit.PublicKey == "" {
		t.Fatalf("circuit missing connection data: %+v", circuit)
	}

	singleHopRecorder := httptest.NewRecorder()
	mux.ServeHTTP(singleHopRecorder, httptest.NewRequest(http.MethodGet, "/circuit?hops=1", nil))
	assertStatus(t, singleHopRecorder, http.StatusOK)

	var singleHop CircuitResponse
	decodeResponse(t, singleHopRecorder, &singleHop)
	if singleHop.Guard.NodeID != guard.NodeID {
		t.Fatalf("single hop guard = %+v, want %s", singleHop.Guard, guard.NodeID)
	}
	if singleHop.Relay.NodeID != "" || singleHop.Exit.NodeID != "" {
		t.Fatalf("single hop response should only include guard: %+v", singleHop)
	}
}

func TestIntegrationEdgeCases(t *testing.T) {
	baseTime := time.Date(2026, time.March, 27, 13, 0, 0, 0, time.UTC)
	server := newTestDirectoryServer(baseTime, 15*time.Second, 30*time.Second)
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	server.randomIntn = func(int) int { return 0 }

	guard := NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174310",
		NodeType:  "guard",
		Host:      "guard-a.local",
		Port:      9101,
		PublicKey: "guard-a-key",
	}
	relay := NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174311",
		NodeType:  "relay",
		Host:      "relay-a.local",
		Port:      9201,
		PublicKey: "relay-a-key",
	}

	performJSONRequest(t, mux, http.MethodPost, "/register", guard)
	performJSONRequest(t, mux, http.MethodPost, "/register", relay)

	updatedGuard := guard
	updatedGuard.Host = "guard-a-updated.local"
	updatedGuard.PublicKey = "guard-a-key-2"
	server.now = func() time.Time { return baseTime.Add(5 * time.Second) }
	updateRecorder := performJSONRequest(t, mux, http.MethodPost, "/register", updatedGuard)
	assertStatus(t, updateRecorder, http.StatusCreated)

	var updatedRecord NodeRecord
	decodeResponse(t, updateRecorder, &updatedRecord)
	if updatedRecord.Host != updatedGuard.Host || updatedRecord.PublicKey != updatedGuard.PublicKey {
		t.Fatalf("updated record = %+v, want host/publicKey from duplicate registration", updatedRecord)
	}

	missingHeartbeatRecorder := httptest.NewRecorder()
	mux.ServeHTTP(missingHeartbeatRecorder, httptest.NewRequest(http.MethodPost, "/heartbeat/123e4567-e89b-12d3-a456-426614174399", nil))
	assertStatus(t, missingHeartbeatRecorder, http.StatusNotFound)

	server.now = func() time.Time { return baseTime.Add(40 * time.Second) }
	server.cleanupUnhealthyNodes(server.now())

	nodesRecorder := httptest.NewRecorder()
	mux.ServeHTTP(nodesRecorder, httptest.NewRequest(http.MethodGet, "/nodes", nil))
	assertStatus(t, nodesRecorder, http.StatusOK)

	var grouped NodesByTypeResponse
	decodeResponse(t, nodesRecorder, &grouped)
	if len(grouped.Guard) != 0 || len(grouped.Relay) != 0 || len(grouped.Exit) != 0 {
		t.Fatalf("grouped nodes = %+v, want all unhealthy nodes excluded", grouped)
	}

	circuitRecorder := httptest.NewRecorder()
	mux.ServeHTTP(circuitRecorder, httptest.NewRequest(http.MethodGet, "/circuit", nil))
	assertStatus(t, circuitRecorder, http.StatusServiceUnavailable)

	server.now = func() time.Time { return baseTime.Add(45 * time.Second) }
	reRegisterRecorder := performJSONRequest(t, mux, http.MethodPost, "/register", updatedGuard)
	assertStatus(t, reRegisterRecorder, http.StatusCreated)

	var reRegistered NodeRecord
	decodeResponse(t, reRegisterRecorder, &reRegistered)
	if reRegistered.Status != StatusHealthy {
		t.Fatalf("status = %q, want %q after re-register", reRegistered.Status, StatusHealthy)
	}
}

func TestIntegrationConcurrentRegistrations(t *testing.T) {
	baseTime := time.Date(2026, time.March, 27, 14, 0, 0, 0, time.UTC)
	server := newTestDirectoryServer(baseTime, 15*time.Second, 30*time.Second)
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)

	registrations := []NodeRegistration{
		{NodeID: "123e4567-e89b-12d3-a456-426614174320", NodeType: "guard", Host: "guard-c1.local", Port: 9301, PublicKey: "g1"},
		{NodeID: "123e4567-e89b-12d3-a456-426614174321", NodeType: "guard", Host: "guard-c2.local", Port: 9302, PublicKey: "g2"},
		{NodeID: "123e4567-e89b-12d3-a456-426614174322", NodeType: "relay", Host: "relay-c1.local", Port: 9401, PublicKey: "r1"},
		{NodeID: "123e4567-e89b-12d3-a456-426614174323", NodeType: "relay", Host: "relay-c2.local", Port: 9402, PublicKey: "r2"},
		{NodeID: "123e4567-e89b-12d3-a456-426614174324", NodeType: "exit", Host: "exit-c1.local", Port: 9501, PublicKey: "e1"},
		{NodeID: "123e4567-e89b-12d3-a456-426614174325", NodeType: "exit", Host: "exit-c2.local", Port: 9502, PublicKey: "e2"},
	}

	var wg sync.WaitGroup
	errCh := make(chan string, len(registrations))

	for _, node := range registrations {
		wg.Add(1)
		go func(node NodeRegistration) {
			defer wg.Done()
			recorder := performJSONRequest(t, mux, http.MethodPost, "/register", node)
			if recorder.Code != http.StatusCreated {
				errCh <- recorder.Body.String()
			}
		}(node)
	}

	wg.Wait()
	close(errCh)

	for errBody := range errCh {
		t.Fatalf("concurrent registration failed: %s", errBody)
	}

	nodesRecorder := httptest.NewRecorder()
	mux.ServeHTTP(nodesRecorder, httptest.NewRequest(http.MethodGet, "/nodes", nil))
	assertStatus(t, nodesRecorder, http.StatusOK)

	var grouped NodesByTypeResponse
	decodeResponse(t, nodesRecorder, &grouped)
	if len(grouped.Guard) != 2 || len(grouped.Relay) != 2 || len(grouped.Exit) != 2 {
		t.Fatalf("grouped nodes = %+v, want 2 guards, 2 relays, 2 exits", grouped)
	}
}

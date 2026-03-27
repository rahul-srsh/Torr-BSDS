package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rahul-srsh/Torr-BSDS/shared/config"
	sharedserver "github.com/rahul-srsh/Torr-BSDS/shared/server"
)

func TestRegisterHandlerCreatesAndUpdatesNode(t *testing.T) {
	handler := newHandler(&config.NodeConfig{
		Port:     "8080",
		NodeType: "directory-server",
	})

	first := NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174000",
		NodeType:  "guard",
		Host:      "10.0.0.10",
		Port:      9001,
		PublicKey: "pubkey-1",
	}

	recorder := performJSONRequest(t, handler, http.MethodPost, "/register", first)
	assertStatus(t, recorder, http.StatusCreated)

	var created NodeRegistration
	decodeResponse(t, recorder, &created)
	if created != first {
		t.Fatalf("created registration = %+v, want %+v", created, first)
	}

	updated := first
	updated.Host = "guard.hopvault.local"
	updated.Port = 9101
	updated.PublicKey = "pubkey-2"

	recorder = performJSONRequest(t, handler, http.MethodPost, "/register", updated)
	assertStatus(t, recorder, http.StatusCreated)

	var saved NodeRegistration
	decodeResponse(t, recorder, &saved)
	if saved != updated {
		t.Fatalf("updated registration = %+v, want %+v", saved, updated)
	}

	debugRecorder := httptest.NewRecorder()
	debugRequest := httptest.NewRequest(http.MethodGet, "/debug/nodes", nil)
	handler.ServeHTTP(debugRecorder, debugRequest)

	assertStatus(t, debugRecorder, http.StatusOK)

	var nodes []NodeRegistration
	decodeResponse(t, debugRecorder, &nodes)
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if nodes[0] != updated {
		t.Fatalf("listed registration = %+v, want %+v", nodes[0], updated)
	}
}

func TestRegisterHandlerRejectsInvalidPayloads(t *testing.T) {
	handler := newHandler(&config.NodeConfig{
		Port:     "8080",
		NodeType: "directory-server",
	})

	tests := []struct {
		name           string
		body           any
		rawBody        string
		expectedStatus int
		expectedError  string
	}{
		{
			name:           "invalid json",
			rawBody:        `{"nodeId":`,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "invalid JSON payload",
		},
		{
			name: "missing node id",
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
			name: "invalid uuid",
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
			name: "missing node type",
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
			name: "invalid node type",
			body: map[string]any{
				"nodeId":    "123e4567-e89b-12d3-a456-426614174001",
				"nodeType":  "middle",
				"host":      "127.0.0.1",
				"port":      8080,
				"publicKey": "pub",
			},
			expectedStatus: http.StatusBadRequest,
			expectedError:  "nodeType must be one of: guard, relay, exit",
		},
		{
			name: "missing host",
			body: map[string]any{
				"nodeId":    "123e4567-e89b-12d3-a456-426614174002",
				"nodeType":  "relay",
				"host":      "",
				"port":      8080,
				"publicKey": "pub",
			},
			expectedStatus: http.StatusBadRequest,
			expectedError:  "host is required",
		},
		{
			name: "invalid port",
			body: map[string]any{
				"nodeId":    "123e4567-e89b-12d3-a456-426614174003",
				"nodeType":  "exit",
				"host":      "127.0.0.1",
				"port":      0,
				"publicKey": "pub",
			},
			expectedStatus: http.StatusBadRequest,
			expectedError:  "port must be between 1 and 65535",
		},
		{
			name: "missing public key",
			body: map[string]any{
				"nodeId":    "123e4567-e89b-12d3-a456-426614174004",
				"nodeType":  "guard",
				"host":      "127.0.0.1",
				"port":      8080,
				"publicKey": "",
			},
			expectedStatus: http.StatusBadRequest,
			expectedError:  "publicKey is required",
		},
		{
			name:           "empty body",
			rawBody:        "",
			expectedStatus: http.StatusBadRequest,
			expectedError:  "invalid JSON payload",
		},
		{
			name:           "multiple json values",
			rawBody:        `{"nodeId":"123e4567-e89b-12d3-a456-426614174011","nodeType":"guard","host":"127.0.0.1","port":8080,"publicKey":"pub"}{"extra":true}`,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "invalid JSON payload",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var body *bytes.Reader
			if tc.rawBody != "" || tc.body == nil {
				body = bytes.NewReader([]byte(tc.rawBody))
			} else {
				payload, err := json.Marshal(tc.body)
				if err != nil {
					t.Fatalf("json.Marshal() error = %v", err)
				}
				body = bytes.NewReader(payload)
			}

			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/register", body)
			handler.ServeHTTP(recorder, req)

			assertStatus(t, recorder, tc.expectedStatus)

			var response map[string]string
			decodeResponse(t, recorder, &response)
			if response["error"] != tc.expectedError {
				t.Fatalf("error = %q, want %q", response["error"], tc.expectedError)
			}
		})
	}
}

func TestDeregisterHandler(t *testing.T) {
	server := NewDirectoryServer(nil)
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)

	node := NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174005",
		NodeType:  "relay",
		Host:      "127.0.0.1",
		Port:      8080,
		PublicKey: "pub",
	}

	performJSONRequest(t, mux, http.MethodPost, "/register", node)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/deregister/"+node.NodeID, nil)
	mux.ServeHTTP(recorder, req)

	assertStatus(t, recorder, http.StatusOK)

	var response map[string]string
	decodeResponse(t, recorder, &response)
	if response["status"] != "deregistered" || response["nodeId"] != node.NodeID {
		t.Fatalf("unexpected deregister response = %+v", response)
	}

	notFoundRecorder := httptest.NewRecorder()
	notFoundRequest := httptest.NewRequest(http.MethodDelete, "/deregister/"+node.NodeID, nil)
	mux.ServeHTTP(notFoundRecorder, notFoundRequest)

	assertStatus(t, notFoundRecorder, http.StatusNotFound)
	decodeResponse(t, notFoundRecorder, &response)
	if response["error"] != "node not found" {
		t.Fatalf("error = %q, want %q", response["error"], "node not found")
	}

	missingIDRecorder := httptest.NewRecorder()
	missingIDRequest := httptest.NewRequest(http.MethodDelete, "/deregister/", nil)
	missingIDRequest.SetPathValue("nodeId", "")
	server.deregisterHandler(missingIDRecorder, missingIDRequest)

	assertStatus(t, missingIDRecorder, http.StatusBadRequest)
	decodeResponse(t, missingIDRecorder, &response)
	if response["error"] != "nodeId is required" {
		t.Fatalf("error = %q, want %q", response["error"], "nodeId is required")
	}
}

func TestNodeRegistryConcurrentUpserts(t *testing.T) {
	registry := NewNodeRegistry()
	const goroutineCount = 32

	var wg sync.WaitGroup
	wg.Add(goroutineCount)

	for i := range goroutineCount {
		go func(i int) {
			defer wg.Done()
			node := NodeRegistration{
				NodeID:    "123e4567-e89b-12d3-a456-426614174100",
				NodeType:  "guard",
				Host:      "127.0.0.1",
				Port:      9000 + i,
				PublicKey: "pub",
			}
			registry.Upsert(node)
		}(i)
	}

	wg.Wait()

	nodes := registry.List()
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if nodes[0].NodeID != "123e4567-e89b-12d3-a456-426614174100" {
		t.Fatalf("nodeId = %q, want %q", nodes[0].NodeID, "123e4567-e89b-12d3-a456-426614174100")
	}

	emptyRegistry := NewNodeRegistry()
	if nodes := emptyRegistry.List(); len(nodes) != 0 {
		t.Fatalf("len(nodes) = %d, want 0", len(nodes))
	}

	sortedRegistry := NewNodeRegistry()
	sortedRegistry.Upsert(NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174102",
		NodeType:  "relay",
		Host:      "127.0.0.1",
		Port:      9102,
		PublicKey: "pub-2",
	})
	sortedRegistry.Upsert(NodeRegistration{
		NodeID:    "123e4567-e89b-12d3-a456-426614174101",
		NodeType:  "guard",
		Host:      "127.0.0.1",
		Port:      9101,
		PublicKey: "pub-1",
	})

	sortedNodes := sortedRegistry.List()
	if len(sortedNodes) != 2 {
		t.Fatalf("len(sortedNodes) = %d, want 2", len(sortedNodes))
	}
	if sortedNodes[0].NodeID != "123e4567-e89b-12d3-a456-426614174101" {
		t.Fatalf("first nodeId = %q, want %q", sortedNodes[0].NodeID, "123e4567-e89b-12d3-a456-426614174101")
	}
	if sortedNodes[1].NodeID != "123e4567-e89b-12d3-a456-426614174102" {
		t.Fatalf("second nodeId = %q, want %q", sortedNodes[1].NodeID, "123e4567-e89b-12d3-a456-426614174102")
	}
}

func TestHelpersAndRun(t *testing.T) {
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

	cfg := &config.NodeConfig{
		Port:     "9090",
		NodeType: "directory-server",
	}

	originalLoadConfig := loadConfig
	originalStartServer := startServer
	defer func() {
		loadConfig = originalLoadConfig
		startServer = originalStartServer
	}()

	loadCalled := false
	startCalled := false

	loadConfig = func() *config.NodeConfig {
		loadCalled = true
		return cfg
	}

	startServer = func(s *sharedserver.BaseServer) {
		startCalled = true

		healthRecorder := httptest.NewRecorder()
		healthRequest := httptest.NewRequest(http.MethodGet, "/health", nil)
		s.Mux.ServeHTTP(healthRecorder, healthRequest)
		assertStatus(t, healthRecorder, http.StatusOK)

		debugRecorder := httptest.NewRecorder()
		debugRequest := httptest.NewRequest(http.MethodGet, "/debug/nodes", nil)
		s.Mux.ServeHTTP(debugRecorder, debugRequest)
		assertStatus(t, debugRecorder, http.StatusOK)
	}

	run()
	if !loadCalled || !startCalled {
		t.Fatalf("run() loadCalled=%t startCalled=%t, want true/true", loadCalled, startCalled)
	}

	loadCalled = false
	startCalled = false
	main()
	if !loadCalled || !startCalled {
		t.Fatalf("main() loadCalled=%t startCalled=%t, want true/true", loadCalled, startCalled)
	}
}

func performJSONRequest(t *testing.T, handler http.Handler, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, req)

	return recorder
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

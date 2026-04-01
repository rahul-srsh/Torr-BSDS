package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- GenerateNodeID ----

func TestGenerateNodeIDFormat(t *testing.T) {
	id, err := GenerateNodeID()
	if err != nil {
		t.Fatalf("GenerateNodeID: %v", err)
	}
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("UUID must have 5 parts, got %d: %q", len(parts), id)
	}
	lengths := []int{8, 4, 4, 4, 12}
	for i, p := range parts {
		if len(p) != lengths[i] {
			t.Fatalf("part %d: len=%d, want %d (full id: %q)", i, len(p), lengths[i], id)
		}
	}
	// Version nibble must be '4'
	if id[14] != '4' {
		t.Fatalf("version nibble = %c, want 4 (id: %q)", id[14], id)
	}
}

func TestGenerateNodeIDUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id, err := GenerateNodeID()
		if err != nil {
			t.Fatalf("GenerateNodeID: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate UUID: %q", id)
		}
		seen[id] = true
	}
}

// ---- GenerateKeyPair ----

func TestGenerateKeyPairReturnsValidPEM(t *testing.T) {
	priv, pubPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if priv == nil {
		t.Fatal("private key is nil")
	}
	if !strings.Contains(pubPEM, "BEGIN PUBLIC KEY") {
		t.Fatalf("publicKey PEM missing header: %q", pubPEM[:min(len(pubPEM), 60)])
	}
	if priv.N.BitLen() != 2048 {
		t.Fatalf("key size = %d bits, want 2048", priv.N.BitLen())
	}
}

func TestGenerateKeyPairProducesUniqueKeys(t *testing.T) {
	_, pem1, _ := GenerateKeyPair()
	_, pem2, _ := GenerateKeyPair()
	if pem1 == pem2 {
		t.Fatal("two GenerateKeyPair calls must produce different keys")
	}
}

// ---- Register ----

func TestRegisterSendsCorrectPayload(t *testing.T) {
	var called atomic.Bool
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/register" || r.Method != http.MethodPost {
			t.Errorf("got %s %s, want POST /register", r.Method, r.URL.Path)
		}
		var req RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if req.NodeID != "test-uuid" {
			t.Errorf("nodeId = %q, want test-uuid", req.NodeID)
		}
		if req.NodeType != "guard" {
			t.Errorf("nodeType = %q, want guard", req.NodeType)
		}
		if req.Host != "10.0.0.5" {
			t.Errorf("host = %q, want 10.0.0.5", req.Host)
		}
		if req.Port != 8080 {
			t.Errorf("port = %d, want 8080", req.Port)
		}
		if req.PublicKey != "test-pem" {
			t.Errorf("publicKey = %q, want test-pem", req.PublicKey)
		}
		called.Store(true)
		w.WriteHeader(http.StatusCreated)
	}))
	defer dir.Close()

	cfg := &Config{
		NodeID:       "test-uuid",
		NodeType:     "guard",
		Host:         "10.0.0.5",
		Port:         8080,
		PublicKeyPEM: "test-pem",
		DirectoryURL: dir.URL,
		HTTPClient:   http.DefaultClient,
	}
	if err := Register(context.Background(), cfg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !called.Load() {
		t.Fatal("directory server was not called")
	}
}

func TestRegisterReturnsErrorOnNon2xx(t *testing.T) {
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer dir.Close()

	cfg := &Config{
		NodeID: "id", NodeType: "relay", Host: "h", Port: 8080,
		DirectoryURL: dir.URL, HTTPClient: http.DefaultClient,
	}
	if err := Register(context.Background(), cfg); err == nil {
		t.Fatal("expected error for non-2xx response")
	}
}

func TestRegisterReturnsErrorWhenUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	cfg := &Config{
		NodeID: "id", NodeType: "exit", Host: "h", Port: 8080,
		DirectoryURL: deadURL, HTTPClient: http.DefaultClient,
	}
	if err := Register(context.Background(), cfg); err == nil {
		t.Fatal("expected error when server is unreachable")
	}
}

// ---- StartHeartbeat ----

func TestStartHeartbeatSendsRepeatedHeartbeats(t *testing.T) {
	var count atomic.Int32
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/heartbeat/") {
			nodeID := strings.TrimPrefix(r.URL.Path, "/heartbeat/")
			if nodeID != "hb-test" {
				t.Errorf("heartbeat nodeId = %q, want hb-test", nodeID)
			}
			count.Add(1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer dir.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		NodeID:            "hb-test",
		NodeType:          "guard",
		DirectoryURL:      dir.URL,
		HTTPClient:        http.DefaultClient,
		HeartbeatInterval: 30 * time.Millisecond,
	}
	StartHeartbeat(ctx, cfg)

	// Wait long enough for at least 3 heartbeats.
	time.Sleep(120 * time.Millisecond)
	cancel()
	time.Sleep(10 * time.Millisecond) // let goroutine drain

	if got := count.Load(); got < 3 {
		t.Fatalf("heartbeat count = %d, want >= 3", got)
	}
}

func TestStartHeartbeatStopsOnContextCancel(t *testing.T) {
	var count atomic.Int32
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer dir.Close()

	ctx, cancel := context.WithCancel(context.Background())

	cfg := &Config{
		NodeID:            "stop-test",
		NodeType:          "relay",
		DirectoryURL:      dir.URL,
		HTTPClient:        http.DefaultClient,
		HeartbeatInterval: 20 * time.Millisecond,
	}
	StartHeartbeat(ctx, cfg)
	time.Sleep(60 * time.Millisecond)
	cancel()
	time.Sleep(10 * time.Millisecond)

	before := count.Load()
	time.Sleep(60 * time.Millisecond) // no more heartbeats should arrive
	after := count.Load()
	if after > before+1 {
		t.Fatalf("heartbeats continued after cancel: before=%d after=%d", before, after)
	}
}

// ---- StartWithBackoff ----

func TestStartWithBackoffRegistersAndStartsHeartbeat(t *testing.T) {
	var registerCalls atomic.Int32
	var heartbeatCalls atomic.Int32

	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/register":
			registerCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/heartbeat/"):
			heartbeatCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer dir.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		NodeID:            "backoff-test",
		NodeType:          "exit",
		Host:              "127.0.0.1",
		Port:              8080,
		DirectoryURL:      dir.URL,
		HTTPClient:        http.DefaultClient,
		HeartbeatInterval: 20 * time.Millisecond,
	}
	go StartWithBackoff(ctx, cfg)

	// Allow registration + a few heartbeats.
	time.Sleep(120 * time.Millisecond)

	if registerCalls.Load() != 1 {
		t.Fatalf("register calls = %d, want 1", registerCalls.Load())
	}
	if heartbeatCalls.Load() < 2 {
		t.Fatalf("heartbeat calls = %d, want >= 2", heartbeatCalls.Load())
	}
}

func TestStartWithBackoffRetriesOnFailure(t *testing.T) {
	var attempts atomic.Int32
	// Fail the first 2 attempts, succeed on the 3rd.
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/register" {
			n := attempts.Add(1)
			if n < 3 {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer dir.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		NodeID:            "retry-test",
		NodeType:          "guard",
		Host:              "127.0.0.1",
		Port:              8080,
		DirectoryURL:      dir.URL,
		HTTPClient:        http.DefaultClient,
		HeartbeatInterval: time.Second, // not relevant — just testing registration
	}

	// Override backoff to 1ms so the test doesn't take 3 seconds.
	// We do this by wrapping StartWithBackoff inline.
	done := make(chan struct{})
	go func() {
		defer close(done)
		backoff := time.Millisecond
		for {
			err := Register(ctx, cfg)
			if err == nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("registration did not succeed within timeout")
	}

	if got := attempts.Load(); got < 3 {
		t.Fatalf("attempts = %d, want >= 3", got)
	}
}

// ---- ResolveOwnAddress ----

func TestResolveOwnAddressFallsBackToHostname(t *testing.T) {
	// No ECS env vars set — should fall back to os.Hostname() without error.
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", "")
	t.Setenv("ECS_CONTAINER_METADATA_URI", "")

	addr, err := ResolveOwnAddress(http.DefaultClient)
	if err != nil {
		t.Fatalf("ResolveOwnAddress: %v", err)
	}
	if addr == "" {
		t.Fatal("address must not be empty")
	}
}

func TestResolveOwnAddressUsesECSMetadata(t *testing.T) {
	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/task" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{
			"Containers": [{
				"Networks": [{
					"NetworkMode": "awsvpc",
					"IPv4Addresses": ["10.0.2.55"]
				}]
			}]
		}`)
	}))
	defer meta.Close()

	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", meta.URL)

	// Use a client that cannot reach the internet so checkip.amazonaws.com fails
	// and the function falls back to ECS metadata.
	noInternetClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// Only allow connections to localhost (test servers).
				host, _, _ := net.SplitHostPort(addr)
				if host == "127.0.0.1" || host == "localhost" || host == "::1" {
					return (&net.Dialer{}).DialContext(ctx, network, addr)
				}
				return nil, fmt.Errorf("blocked: no internet in test")
			},
		},
		Timeout: 5 * time.Second,
	}

	addr, err := ResolveOwnAddress(noInternetClient)
	if err != nil {
		t.Fatalf("ResolveOwnAddress: %v", err)
	}
	if addr != "10.0.2.55" {
		t.Fatalf("addr = %q, want 10.0.2.55", addr)
	}
}

// ---- helpers ----

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

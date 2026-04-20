package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rahul-srsh/Torr-BSDS/shared/config"
	"github.com/rahul-srsh/Torr-BSDS/shared/node"
	onion "github.com/rahul-srsh/Torr-BSDS/shared/onion"
	sharedserver "github.com/rahul-srsh/Torr-BSDS/shared/server"
)

// TestRelayOnionRoundTrip verifies that the relay decrypts its layer,
// forwards the inner payload to the exit node, and re-encrypts the response.
// The relay knows only the previous hop (guard) and next hop (exit) —
// not the original client or the final destination.
func TestRelayOnionRoundTrip(t *testing.T) {
	relayKey := randomRelayKey(t)
	exitEncryptedBytes := []byte("exit-node-encrypted-response")

	// Mock exit node.
	exit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/onion" {
			t.Errorf("exit: got %s %s, want POST /onion", r.Method, r.URL.Path)
		}
		var req onion.OnionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("exit: decode request: %v", err)
		}
		if req.CircuitID != "relay-test" {
			t.Errorf("exit: circuitId = %q, want relay-test", req.CircuitID)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(onion.OnionResponse{
				Payload: exitEncryptedBytes,
			})
		}))
	defer exit.Close()

	exitAddr := strings.TrimPrefix(exit.URL, "http://")

	// Build relay layer: after decryption the relay sees nextHop=exit and inner payload.
	layer := onion.Layer{NextHop: exitAddr, Payload: []byte("exit-encrypted-layer")}
	layerJSON, _ := json.Marshal(layer)

	ct, err := onion.Encrypt(relayKey, layerJSON)
	if err != nil {
		t.Fatalf("Encrypt relay layer: %v", err)
	}

	ks := onion.NewKeyStore()
	ks.Store("relay-test", relayKey)
	h := onion.NewHandler(ks, http.DefaultClient, "relay")

	body, _ := json.Marshal(onion.OnionRequest{
		CircuitID: "relay-test",
		Payload:   ct,
	})
	// RemoteAddr simulates the guard node's address.
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	r.RemoteAddr = "10.0.0.1:12345" // guard IP
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp onion.OnionResponse
	json.NewDecoder(w.Body).Decode(&resp)

	// Guard (or client in this test) peels the relay layer.
	plaintext, err := onion.Decrypt(relayKey, resp.Payload)
	if err != nil {
		t.Fatalf("Decrypt relay layer: %v", err)
	}
	if !bytes.Equal(plaintext, exitEncryptedBytes) {
		t.Fatalf("plaintext = %q, want %q", plaintext, exitEncryptedBytes)
	}
}

// TestRelayKeyEndpoint verifies POST /key stores the session key.
func TestRelayKeyEndpoint(t *testing.T) {
	key := randomRelayKey(t)
	ks := onion.NewKeyStore()
	h := onion.NewHandler(ks, http.DefaultClient, "relay")

	body, _ := json.Marshal(onion.KeyRequest{
		CircuitID: "r1",
		Key:       base64.StdEncoding.EncodeToString(key),
	})
	r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleKey(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	stored, ok := ks.Get("r1")
	if !ok || !bytes.Equal(stored, key) {
		t.Fatal("key not stored correctly")
	}
}

// TestRelayUnknownCircuit verifies the relay rejects requests with no registered key.
func TestRelayUnknownCircuit(t *testing.T) {
	h := onion.NewHandler(onion.NewKeyStore(), http.DefaultClient, "relay")
	body, _ := json.Marshal(onion.OnionRequest{CircuitID: "unknown", Payload: []byte("abc")})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// TestRelayNextHopUnreachable verifies the relay returns 502 when exit is down.
func TestRelayNextHopUnreachable(t *testing.T) {
	key := randomRelayKey(t)
	ks := onion.NewKeyStore()
	ks.Store("r1", key)

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadAddr := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()

	layer := onion.Layer{NextHop: deadAddr, Payload: []byte("inner")}
	layerJSON, _ := json.Marshal(layer)
	ct, _ := onion.Encrypt(key, layerJSON)

	h := onion.NewHandler(ks, http.DefaultClient, "relay")
	body, _ := json.Marshal(onion.OnionRequest{
		CircuitID: "r1",
		Payload:   ct,
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func randomRelayKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return key
}

func fakeRelayIdentity(t *testing.T) *node.Identity {
	t.Helper()
	priv, pub, err := node.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	return &node.Identity{NodeID: "test-node-id", PrivateKey: priv, PublicKeyPEM: pub, Host: "127.0.0.1"}
}

func overrideRelayHooks(t *testing.T, identity *node.Identity, idErr error) (called *bool, started **sharedserver.BaseServer) {
	t.Helper()
	origLoad := loadConfig
	origNew := newIdentity
	origStart := startServer
	origReg := startRegistration
	t.Cleanup(func() {
		loadConfig = origLoad
		newIdentity = origNew
		startServer = origStart
		startRegistration = origReg
	})

	loadConfig = func() *config.NodeConfig {
		return &config.NodeConfig{Port: "0", NodeType: "relay", DirectoryServerURL: "http://localhost:1"}
	}
	newIdentity = func() (*node.Identity, error) { return identity, idErr }

	var reg bool
	startRegistration = func(context.Context, *node.Config) { reg = true }

	var srv *sharedserver.BaseServer
	startServer = func(s *sharedserver.BaseServer) { srv = s }

	return &reg, &srv
}

func TestRunWiresUpRelayRoutes(t *testing.T) {
	regCalled, started := overrideRelayHooks(t, fakeRelayIdentity(t), nil)

	if err := run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !*regCalled {
		t.Fatal("startRegistration was not called")
	}
	if *started == nil {
		t.Fatal("startServer was not called")
	}

	for _, path := range []string{"/health", "/key", "/onion", "/setup"} {
		recorder := httptest.NewRecorder()
		(*started).Mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code == http.StatusNotFound {
			t.Errorf("route %s not registered", path)
		}
	}
}

func TestRunPropagatesIdentityError(t *testing.T) {
	overrideRelayHooks(t, nil, errIdentityFailed)
	if err := run(); err != errIdentityFailed {
		t.Fatalf("run() err = %v, want %v", err, errIdentityFailed)
	}
}

func TestMainDelegatesToRun(t *testing.T) {
	overrideRelayHooks(t, fakeRelayIdentity(t), nil)
	main() // returns because startServer is a no-op
}

var errIdentityFailed = &identityFailedError{}

type identityFailedError struct{}

func (*identityFailedError) Error() string { return "identity failed" }

func TestDefaultHooksAreExecutable(t *testing.T) {
	id, err := defaultNewIdentity()
	if err != nil {
		t.Fatalf("defaultNewIdentity: %v", err)
	}
	if id.NodeID == "" || id.PrivateKey == nil {
		t.Fatal("default identity not populated")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	defaultStartRegistration(ctx, &node.Config{
		NodeID:            id.NodeID,
		NodeType:          "relay",
		DirectoryURL:      "http://127.0.0.1:1",
		HTTPClient:        &http.Client{Timeout: 10 * time.Millisecond},
		HeartbeatInterval: time.Hour,
	})

	srv := sharedserver.New(&config.NodeConfig{Port: freePort(t), NodeType: "relay-default"})
	done := make(chan struct{})
	go func() { defer close(done); defaultStartServer(srv) }()
	waitForServer(t, "http://127.0.0.1:"+srv.Config.Port+"/health")
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := listener.Addr().(*net.TCPAddr)
	listener.Close()
	return strconv.Itoa(addr.Port)
}

func waitForServer(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s did not become healthy", url)
}

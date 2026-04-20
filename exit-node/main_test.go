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
	"testing"
	"time"

	"github.com/rahul-srsh/Torr-BSDS/shared/config"
	"github.com/rahul-srsh/Torr-BSDS/shared/node"
	onion "github.com/rahul-srsh/Torr-BSDS/shared/onion"
	sharedserver "github.com/rahul-srsh/Torr-BSDS/shared/server"
)

// TestExitNodeRoundTrip verifies the exit node decrypts the final layer,
// executes the HTTP request, and returns an encrypted ExitResponse.
func TestExitNodeRoundTrip(t *testing.T) {
	key := randomExitKey(t)

	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer dest.Close()

	layer := onion.ExitLayer{
		URL:    dest.URL + "/api",
		Method: http.MethodGet,
	}
	layerJSON, _ := json.Marshal(layer)
	ct, err := onion.Encrypt(key, layerJSON)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	ks := onion.NewKeyStore()
	ks.Store("exit-1", key)
	h := onion.NewExitHandler(ks, http.DefaultClient)

	body, _ := json.Marshal(onion.OnionRequest{
		CircuitID: "exit-1",
		Payload:   ct,
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp onion.OnionResponse
	json.NewDecoder(w.Body).Decode(&resp)

	// Relay decrypts with exit key.
	plaintext, err := onion.Decrypt(key, resp.Payload)
	if err != nil {
		t.Fatalf("Decrypt exit response: %v", err)
	}

	var exitResp onion.ExitResponse
	if err := json.Unmarshal(plaintext, &exitResp); err != nil {
		t.Fatalf("unmarshal ExitResponse: %v", err)
	}
	if exitResp.StatusCode != http.StatusOK {
		t.Fatalf("inner status = %d, want %d", exitResp.StatusCode, http.StatusOK)
	}
	if string(exitResp.Body) != `{"status":"ok"}` {
		t.Fatalf("body = %q, want {\"status\":\"ok\"}", exitResp.Body)
	}
}

// TestExitNodeKeyRegistration verifies POST /key stores the session key.
func TestExitNodeKeyRegistration(t *testing.T) {
	key := randomExitKey(t)
	ks := onion.NewKeyStore()
	h := onion.NewExitHandler(ks, http.DefaultClient)

	body, _ := json.Marshal(onion.KeyRequest{
		CircuitID: "exit-1",
		Key:       base64.StdEncoding.EncodeToString(key),
	})
	r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleKey(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	stored, ok := ks.Get("exit-1")
	if !ok || !bytes.Equal(stored, key) {
		t.Fatal("key not stored correctly")
	}
}

// TestExitNodeNon200PropagatesThrough verifies that non-200 destination responses
// are encrypted and returned rather than causing a circuit-level error.
func TestExitNodeNon200PropagatesThrough(t *testing.T) {
	key := randomExitKey(t)

	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("service down"))
	}))
	defer dest.Close()

	layer := onion.ExitLayer{URL: dest.URL, Method: http.MethodGet}
	layerJSON, _ := json.Marshal(layer)
	ct, _ := onion.Encrypt(key, layerJSON)

	ks := onion.NewKeyStore()
	ks.Store("exit-2", key)
	h := onion.NewExitHandler(ks, http.DefaultClient)

	body, _ := json.Marshal(onion.OnionRequest{
		CircuitID: "exit-2",
		Payload:   ct,
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("outer status = %d, want 200 (503 travels inside the encrypted response)", w.Code)
	}

	var resp onion.OnionResponse
	json.NewDecoder(w.Body).Decode(&resp)
	plaintext, _ := onion.Decrypt(key, resp.Payload)

	var exitResp onion.ExitResponse
	json.Unmarshal(plaintext, &exitResp)
	if exitResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("inner status = %d, want %d", exitResp.StatusCode, http.StatusServiceUnavailable)
	}
}

func randomExitKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return key
}

func fakeExitIdentity(t *testing.T) *node.Identity {
	t.Helper()
	priv, pub, err := node.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	return &node.Identity{NodeID: "test-node-id", PrivateKey: priv, PublicKeyPEM: pub, Host: "127.0.0.1"}
}

func overrideExitHooks(t *testing.T, identity *node.Identity, idErr error) (called *bool, started **sharedserver.BaseServer) {
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
		return &config.NodeConfig{Port: "0", NodeType: "exit", DirectoryServerURL: "http://localhost:1"}
	}
	newIdentity = func() (*node.Identity, error) { return identity, idErr }

	var reg bool
	startRegistration = func(context.Context, *node.Config) { reg = true }

	var srv *sharedserver.BaseServer
	startServer = func(s *sharedserver.BaseServer) { srv = s }

	return &reg, &srv
}

func TestRunWiresUpExitRoutes(t *testing.T) {
	regCalled, started := overrideExitHooks(t, fakeExitIdentity(t), nil)

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
	overrideExitHooks(t, nil, errIdentityFailed)
	if err := run(); err != errIdentityFailed {
		t.Fatalf("run() err = %v, want %v", err, errIdentityFailed)
	}
}

func TestMainDelegatesToRun(t *testing.T) {
	overrideExitHooks(t, fakeExitIdentity(t), nil)
	main()
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
		NodeType:          "exit",
		DirectoryURL:      "http://127.0.0.1:1",
		HTTPClient:        &http.Client{Timeout: 10 * time.Millisecond},
		HeartbeatInterval: time.Hour,
	})

	srv := sharedserver.New(&config.NodeConfig{Port: freePort(t), NodeType: "exit-default"})
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

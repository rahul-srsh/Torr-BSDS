package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	onion "github.com/rahul-srsh/Torr-BSDS/shared/onion"
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

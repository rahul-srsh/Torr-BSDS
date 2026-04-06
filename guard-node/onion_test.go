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

// TestGuardOnionRoundTrip verifies that the guard's /onion route correctly
// decrypts, forwards, and re-encrypts through a mock relay.
func TestGuardOnionRoundTrip(t *testing.T) {
	key := randomGuardKey(t)
	relayPayload := []byte("relay-layer-encrypted-data")

	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(onion.OnionResponse{
			Payload: relayPayload,
		})
	}))
	defer relay.Close()

	relayAddr := strings.TrimPrefix(relay.URL, "http://")

	layer := onion.Layer{
		NextHop: relayAddr,
		Payload: []byte("inner"),
	}
	layerJSON, _ := json.Marshal(layer)
	ct, err := onion.Encrypt(key, layerJSON)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	ks := onion.NewKeyStore()
	ks.Store("g1", key)
	h := onion.NewHandler(ks, http.DefaultClient, "guard")

	body, _ := json.Marshal(onion.OnionRequest{
		CircuitID: "g1",
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

	plaintext, err := onion.Decrypt(key, resp.Payload)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(plaintext, relayPayload) {
		t.Fatalf("plaintext = %q, want %q", plaintext, relayPayload)
	}
}

// TestGuardKeyEndpoint verifies the /key route stores session keys.
func TestGuardKeyEndpoint(t *testing.T) {
	key := randomGuardKey(t)
	ks := onion.NewKeyStore()
	h := onion.NewHandler(ks, http.DefaultClient, "guard")

	body, _ := json.Marshal(onion.KeyRequest{
		CircuitID: "g1",
		Key:       base64.StdEncoding.EncodeToString(key),
	})
	r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleKey(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	stored, ok := ks.Get("g1")
	if !ok || !bytes.Equal(stored, key) {
		t.Fatal("key not stored correctly")
	}
}

func randomGuardKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return key
}

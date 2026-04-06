package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	onion "github.com/rahul-srsh/Torr-BSDS/shared/onion"
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

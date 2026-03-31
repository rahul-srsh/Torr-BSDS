package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ---- encrypt / decrypt ----

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := randomKey(t)
	plaintext := []byte("hello, onion routing")

	ciphertext, err := encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	got, err := decrypt(key, ciphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("got %q, want %q", got, plaintext)
	}
}

func TestEncryptProducesDistinctCiphertexts(t *testing.T) {
	key := randomKey(t)
	plaintext := []byte("same plaintext")

	ct1, _ := encrypt(key, plaintext)
	ct2, _ := encrypt(key, plaintext)
	if bytes.Equal(ct1, ct2) {
		t.Fatal("two encryptions of the same plaintext must differ (random nonce)")
	}
}

func TestDecryptCiphertextTooShort(t *testing.T) {
	key := randomKey(t)
	_, err := decrypt(key, []byte("short"))
	if err == nil {
		t.Fatal("expected error for short ciphertext")
	}
}

func TestDecryptWrongKey(t *testing.T) {
	key1 := randomKey(t)
	key2 := randomKey(t)

	ciphertext, _ := encrypt(key1, []byte("secret"))
	_, err := decrypt(key2, ciphertext)
	if err == nil {
		t.Fatal("expected error when decrypting with wrong key")
	}
}

func TestDecryptInvalidKeyLength(t *testing.T) {
	_, err := decrypt([]byte("short"), []byte("anyciphertext"))
	if err == nil {
		t.Fatal("expected error for invalid key length")
	}
}

// ---- keyStore ----

func TestKeyStoreStoreAndGet(t *testing.T) {
	ks := newKeyStore()
	key := randomKey(t)

	ks.store("circuit-1", key)

	got, ok := ks.get("circuit-1")
	if !ok {
		t.Fatal("key not found after store")
	}
	if !bytes.Equal(got, key) {
		t.Fatal("retrieved key differs from stored key")
	}
}

func TestKeyStoreGetMissing(t *testing.T) {
	ks := newKeyStore()
	_, ok := ks.get("missing-circuit")
	if ok {
		t.Fatal("expected not found for unknown circuit")
	}
}

func TestKeyStoreIsolatesKeys(t *testing.T) {
	ks := newKeyStore()
	key := randomKey(t)
	ks.store("circuit-1", key)

	// Mutate the original slice — stored copy must be unaffected.
	key[0] ^= 0xFF

	got, _ := ks.get("circuit-1")
	if got[0] == key[0] {
		t.Fatal("keyStore must store an independent copy of the key")
	}
}

func TestKeyStoreConcurrentAccess(t *testing.T) {
	ks := newKeyStore()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "circuit"
			ks.store(id, randomKey(t))
			ks.get(id)
		}(i)
	}
	wg.Wait()
}

// ---- handleKey ----

func TestHandleKeySuccess(t *testing.T) {
	h := newOnionHandler(newKeyStore(), http.DefaultClient)
	key := randomKey(t)

	body, _ := json.Marshal(KeyRequest{
		CircuitID: "c1",
		Key:       base64.StdEncoding.EncodeToString(key),
	})
	r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.handleKey(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	stored, ok := h.keys.get("c1")
	if !ok || !bytes.Equal(stored, key) {
		t.Fatal("key not stored correctly")
	}
}

func TestHandleKeyMethodNotAllowed(t *testing.T) {
	h := newOnionHandler(newKeyStore(), http.DefaultClient)
	r := httptest.NewRequest(http.MethodGet, "/key", nil)
	w := httptest.NewRecorder()
	h.handleKey(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleKeyInvalidJSON(t *testing.T) {
	h := newOnionHandler(newKeyStore(), http.DefaultClient)
	r := httptest.NewRequest(http.MethodPost, "/key", strings.NewReader("{bad json"))
	w := httptest.NewRecorder()
	h.handleKey(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleKeyMissingFields(t *testing.T) {
	h := newOnionHandler(newKeyStore(), http.DefaultClient)

	for _, tc := range []struct {
		name string
		body KeyRequest
	}{
		{"missing circuitId", KeyRequest{Key: base64.StdEncoding.EncodeToString(randomKey(t))}},
		{"missing key", KeyRequest{CircuitID: "c1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.body)
			r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
			w := httptest.NewRecorder()
			h.handleKey(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestHandleKeyInvalidBase64(t *testing.T) {
	h := newOnionHandler(newKeyStore(), http.DefaultClient)
	body, _ := json.Marshal(KeyRequest{CircuitID: "c1", Key: "!!!not-base64!!!"})
	r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.handleKey(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleKeyWrongKeyLength(t *testing.T) {
	h := newOnionHandler(newKeyStore(), http.DefaultClient)
	shortKey := make([]byte, 16) // 16 bytes — not 32
	body, _ := json.Marshal(KeyRequest{
		CircuitID: "c1",
		Key:       base64.StdEncoding.EncodeToString(shortKey),
	})
	r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.handleKey(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// ---- handleOnion ----

func TestHandleOnionSuccess(t *testing.T) {
	key := randomKey(t)

	// Bytes the relay would return (already relay-encrypted in a real circuit).
	relayEncryptedBytes := []byte("relay-encrypted-response-data")

	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/onion" {
			t.Errorf("relay: got %s %s, want POST /onion", r.Method, r.URL.Path)
		}
		var req OnionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("relay: decode request: %v", err)
		}
		if req.CircuitID != "test-circuit" {
			t.Errorf("relay: circuitId = %q, want %q", req.CircuitID, "test-circuit")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(OnionResponse{
			Payload: base64.StdEncoding.EncodeToString(relayEncryptedBytes),
		})
	}))
	defer relay.Close()

	relayAddr := strings.TrimPrefix(relay.URL, "http://")

	// Build the guard layer: {nextHop, payload}.
	innerPayload := base64.StdEncoding.EncodeToString([]byte("inner-encrypted-data"))
	layer := Layer{NextHop: relayAddr, Payload: innerPayload}
	layerJSON, _ := json.Marshal(layer)

	ciphertext, err := encrypt(key, layerJSON)
	if err != nil {
		t.Fatalf("encrypt layer: %v", err)
	}

	ks := newKeyStore()
	ks.store("test-circuit", key)
	h := newOnionHandler(ks, http.DefaultClient)

	body, _ := json.Marshal(OnionRequest{
		CircuitID: "test-circuit",
		Payload:   base64.StdEncoding.EncodeToString(ciphertext),
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.handleOnion(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp OnionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Client peels the guard layer to recover the relay's payload.
	respCT, err := base64.StdEncoding.DecodeString(resp.Payload)
	if err != nil {
		t.Fatalf("decode resp payload: %v", err)
	}
	plaintext, err := decrypt(key, respCT)
	if err != nil {
		t.Fatalf("decrypt guard layer: %v", err)
	}
	if !bytes.Equal(plaintext, relayEncryptedBytes) {
		t.Fatalf("plaintext = %q, want %q", plaintext, relayEncryptedBytes)
	}
}

func TestHandleOnionMethodNotAllowed(t *testing.T) {
	h := newOnionHandler(newKeyStore(), http.DefaultClient)
	r := httptest.NewRequest(http.MethodGet, "/onion", nil)
	w := httptest.NewRecorder()
	h.handleOnion(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleOnionInvalidJSON(t *testing.T) {
	h := newOnionHandler(newKeyStore(), http.DefaultClient)
	r := httptest.NewRequest(http.MethodPost, "/onion", strings.NewReader("{bad"))
	w := httptest.NewRecorder()
	h.handleOnion(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleOnionMissingFields(t *testing.T) {
	h := newOnionHandler(newKeyStore(), http.DefaultClient)

	for _, tc := range []struct {
		name string
		body OnionRequest
	}{
		{"missing circuitId", OnionRequest{Payload: "abc"}},
		{"missing payload", OnionRequest{CircuitID: "c1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.body)
			r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
			w := httptest.NewRecorder()
			h.handleOnion(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestHandleOnionUnknownCircuit(t *testing.T) {
	h := newOnionHandler(newKeyStore(), http.DefaultClient)
	body, _ := json.Marshal(OnionRequest{CircuitID: "unknown", Payload: "abc"})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.handleOnion(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHandleOnionInvalidBase64Payload(t *testing.T) {
	ks := newKeyStore()
	ks.store("c1", randomKey(t))
	h := newOnionHandler(ks, http.DefaultClient)

	body, _ := json.Marshal(OnionRequest{CircuitID: "c1", Payload: "!!!not-base64!!!"})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.handleOnion(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleOnionDecryptionFailure(t *testing.T) {
	ks := newKeyStore()
	ks.store("c1", randomKey(t)) // register one key

	// Encrypt with a *different* key — decryption will fail.
	ct, _ := encrypt(randomKey(t), []byte("data"))
	h := newOnionHandler(ks, http.DefaultClient)

	body, _ := json.Marshal(OnionRequest{
		CircuitID: "c1",
		Payload:   base64.StdEncoding.EncodeToString(ct),
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.handleOnion(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleOnionInvalidLayerFormat(t *testing.T) {
	key := randomKey(t)
	ks := newKeyStore()
	ks.store("c1", key)
	h := newOnionHandler(ks, http.DefaultClient)

	// Encrypt something that is not a valid Layer JSON.
	ct, _ := encrypt(key, []byte("not valid json"))
	body, _ := json.Marshal(OnionRequest{
		CircuitID: "c1",
		Payload:   base64.StdEncoding.EncodeToString(ct),
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.handleOnion(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleOnionRelayUnreachable(t *testing.T) {
	key := randomKey(t)
	ks := newKeyStore()
	ks.store("c1", key)
	h := newOnionHandler(ks, http.DefaultClient)

	// Point to a server that has been stopped.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadAddr := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()

	layer := Layer{NextHop: deadAddr, Payload: base64.StdEncoding.EncodeToString([]byte("inner"))}
	layerJSON, _ := json.Marshal(layer)
	ct, _ := encrypt(key, layerJSON)

	body, _ := json.Marshal(OnionRequest{
		CircuitID: "c1",
		Payload:   base64.StdEncoding.EncodeToString(ct),
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.handleOnion(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func TestHandleOnionRelayReturnsError(t *testing.T) {
	key := randomKey(t)
	ks := newKeyStore()
	ks.store("c1", key)

	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "relay internal error", http.StatusInternalServerError)
	}))
	defer relay.Close()

	relayAddr := strings.TrimPrefix(relay.URL, "http://")
	layer := Layer{NextHop: relayAddr, Payload: base64.StdEncoding.EncodeToString([]byte("inner"))}
	layerJSON, _ := json.Marshal(layer)
	ct, _ := encrypt(key, layerJSON)

	h := newOnionHandler(ks, http.DefaultClient)
	body, _ := json.Marshal(OnionRequest{
		CircuitID: "c1",
		Payload:   base64.StdEncoding.EncodeToString(ct),
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.handleOnion(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

// ---- helpers ----

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return key
}

package onion

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

// ---- Encrypt / Decrypt ----

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := randomKey(t)
	plaintext := []byte("hello, onion routing")

	ciphertext, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := Decrypt(key, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("got %q, want %q", got, plaintext)
	}
}

func TestEncryptProducesDistinctCiphertexts(t *testing.T) {
	key := randomKey(t)
	ct1, _ := Encrypt(key, []byte("same"))
	ct2, _ := Encrypt(key, []byte("same"))
	if bytes.Equal(ct1, ct2) {
		t.Fatal("two encryptions of the same plaintext must differ (random nonce)")
	}
}

func TestDecryptCiphertextTooShort(t *testing.T) {
	_, err := Decrypt(randomKey(t), []byte("short"))
	if err == nil {
		t.Fatal("expected error for short ciphertext")
	}
}

func TestDecryptWrongKey(t *testing.T) {
	ct, _ := Encrypt(randomKey(t), []byte("secret"))
	_, err := Decrypt(randomKey(t), ct)
	if err == nil {
		t.Fatal("expected error when decrypting with wrong key")
	}
}

func TestDecryptInvalidKeyLength(t *testing.T) {
	_, err := Decrypt([]byte("tooshort"), []byte("anyciphertext"))
	if err == nil {
		t.Fatal("expected error for invalid key length")
	}
}

// ---- KeyStore ----

func TestKeyStoreStoreAndGet(t *testing.T) {
	ks := NewKeyStore()
	key := randomKey(t)
	ks.Store("c1", key)

	got, ok := ks.Get("c1")
	if !ok {
		t.Fatal("key not found after Store")
	}
	if !bytes.Equal(got, key) {
		t.Fatal("retrieved key differs from stored key")
	}
}

func TestKeyStoreGetMissing(t *testing.T) {
	_, ok := NewKeyStore().Get("missing")
	if ok {
		t.Fatal("expected not found for unknown circuit")
	}
}

func TestKeyStoreIsolatesKeys(t *testing.T) {
	ks := NewKeyStore()
	key := randomKey(t)
	ks.Store("c1", key)

	original := key[0]
	key[0] ^= 0xFF // mutate original slice

	got, _ := ks.Get("c1")
	if got[0] != original {
		t.Fatal("KeyStore must store an independent copy")
	}
}

func TestKeyStoreConcurrentAccess(t *testing.T) {
	ks := NewKeyStore()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ks.Store("c1", randomKey(t))
			ks.Get("c1")
		}()
	}
	wg.Wait()
}

// ---- HandleKey ----

func TestHandleKeySuccess(t *testing.T) {
	h := NewHandler(NewKeyStore(), http.DefaultClient, "test")
	key := randomKey(t)

	body, _ := json.Marshal(KeyRequest{
		CircuitID: "c1",
		Key:       base64.StdEncoding.EncodeToString(key),
	})
	r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleKey(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	stored, ok := h.Keys.Get("c1")
	if !ok || !bytes.Equal(stored, key) {
		t.Fatal("key not stored correctly")
	}
}

func TestHandleKeyMethodNotAllowed(t *testing.T) {
	h := NewHandler(NewKeyStore(), http.DefaultClient, "test")
	r := httptest.NewRequest(http.MethodGet, "/key", nil)
	w := httptest.NewRecorder()
	h.HandleKey(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleKeyInvalidJSON(t *testing.T) {
	h := NewHandler(NewKeyStore(), http.DefaultClient, "test")
	r := httptest.NewRequest(http.MethodPost, "/key", strings.NewReader("{bad"))
	w := httptest.NewRecorder()
	h.HandleKey(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleKeyMissingFields(t *testing.T) {
	h := NewHandler(NewKeyStore(), http.DefaultClient, "test")
	cases := []KeyRequest{
		{Key: base64.StdEncoding.EncodeToString(randomKey(t))}, // missing circuitId
		{CircuitID: "c1"},                                       // missing key
	}
	for _, req := range cases {
		body, _ := json.Marshal(req)
		r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
		w := httptest.NewRecorder()
		h.HandleKey(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	}
}

func TestHandleKeyInvalidBase64(t *testing.T) {
	h := NewHandler(NewKeyStore(), http.DefaultClient, "test")
	body, _ := json.Marshal(KeyRequest{CircuitID: "c1", Key: "!!!not-base64!!!"})
	r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleKey(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleKeyWrongKeyLength(t *testing.T) {
	h := NewHandler(NewKeyStore(), http.DefaultClient, "test")
	shortKey := make([]byte, 16)
	body, _ := json.Marshal(KeyRequest{
		CircuitID: "c1",
		Key:       base64.StdEncoding.EncodeToString(shortKey),
	})
	r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleKey(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// ---- HandleOnion ----

func TestHandleOnionSuccess(t *testing.T) {
	key := randomKey(t)
	exitEncryptedBytes := []byte("exit-encrypted-response-data")

	nextHop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/onion" {
			t.Errorf("next hop: got %s %s", r.Method, r.URL.Path)
		}
		var req OnionRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.CircuitID != "test-circuit" {
			t.Errorf("circuitId = %q, want test-circuit", req.CircuitID)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(OnionResponse{
				Payload: exitEncryptedBytes,
			})
		}))
	defer nextHop.Close()

	nextAddr := strings.TrimPrefix(nextHop.URL, "http://")
	layer := Layer{NextHop: nextAddr, Payload: []byte("inner-encrypted-data")}
	layerJSON, _ := json.Marshal(layer)
	ct, err := Encrypt(key, layerJSON)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	ks := NewKeyStore()
	ks.Store("test-circuit", key)
	h := NewHandler(ks, http.DefaultClient, "guard")

	body, _ := json.Marshal(OnionRequest{
		CircuitID: "test-circuit",
		Payload:   ct,
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp OnionResponse
	json.NewDecoder(w.Body).Decode(&resp)

	plaintext, err := Decrypt(key, resp.Payload)
	if err != nil {
		t.Fatalf("Decrypt response: %v", err)
	}
	if !bytes.Equal(plaintext, exitEncryptedBytes) {
		t.Fatalf("plaintext = %q, want %q", plaintext, exitEncryptedBytes)
	}
}

func TestHandleOnionMethodNotAllowed(t *testing.T) {
	h := NewHandler(NewKeyStore(), http.DefaultClient, "test")
	r := httptest.NewRequest(http.MethodGet, "/onion", nil)
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleOnionInvalidJSON(t *testing.T) {
	h := NewHandler(NewKeyStore(), http.DefaultClient, "test")
	r := httptest.NewRequest(http.MethodPost, "/onion", strings.NewReader("{bad"))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleOnionMissingFields(t *testing.T) {
	h := NewHandler(NewKeyStore(), http.DefaultClient, "test")
	cases := []OnionRequest{
		{Payload: []byte("abc")}, // missing circuitId
		{CircuitID: "c1"},        // missing payload
	}
	for _, req := range cases {
		body, _ := json.Marshal(req)
		r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
		w := httptest.NewRecorder()
		h.HandleOnion(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	}
}

func TestHandleOnionUnknownCircuit(t *testing.T) {
	h := NewHandler(NewKeyStore(), http.DefaultClient, "test")
	body, _ := json.Marshal(OnionRequest{CircuitID: "unknown", Payload: []byte("abc")})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHandleOnionDecryptionFailure(t *testing.T) {
	ks := NewKeyStore()
	ks.Store("c1", randomKey(t))
	ct, _ := Encrypt(randomKey(t), []byte("data")) // encrypted with a different key

	h := NewHandler(ks, http.DefaultClient, "test")
	body, _ := json.Marshal(OnionRequest{
		CircuitID: "c1",
		Payload:   ct,
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleOnionInvalidLayerFormat(t *testing.T) {
	key := randomKey(t)
	ks := NewKeyStore()
	ks.Store("c1", key)
	ct, _ := Encrypt(key, []byte("not valid json"))

	h := NewHandler(ks, http.DefaultClient, "test")
	body, _ := json.Marshal(OnionRequest{
		CircuitID: "c1",
		Payload:   ct,
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleOnionNextHopUnreachable(t *testing.T) {
	key := randomKey(t)
	ks := NewKeyStore()
	ks.Store("c1", key)

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadAddr := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()

	layer := Layer{NextHop: deadAddr, Payload: []byte("inner")}
	layerJSON, _ := json.Marshal(layer)
	ct, _ := Encrypt(key, layerJSON)

	h := NewHandler(ks, http.DefaultClient, "test")
	body, _ := json.Marshal(OnionRequest{
		CircuitID: "c1",
		Payload:   ct,
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func TestHandleOnionNextHopReturnsError(t *testing.T) {
	key := randomKey(t)
	ks := NewKeyStore()
	ks.Store("c1", key)

	nextHop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer nextHop.Close()

	layer := Layer{
		NextHop: strings.TrimPrefix(nextHop.URL, "http://"),
		Payload: []byte("inner"),
	}
	layerJSON, _ := json.Marshal(layer)
	ct, _ := Encrypt(key, layerJSON)

	h := NewHandler(ks, http.DefaultClient, "test")
	body, _ := json.Marshal(OnionRequest{
		CircuitID: "c1",
		Payload:   ct,
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
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

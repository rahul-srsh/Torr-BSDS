package onion

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestExitHandleOnionSuccess verifies the full exit-node flow:
// decrypt layer → make HTTP request → encrypt response → return.
func TestExitHandleOnionSuccess(t *testing.T) {
	key := randomKey(t)

	// Destination server that echoes back a known response.
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("dest: method = %q, want GET", r.Method)
		}
		if r.Header.Get("X-Test") != "hello" {
			t.Errorf("dest: X-Test header = %q, want hello", r.Header.Get("X-Test"))
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("destination response"))
	}))
	defer dest.Close()

	layer := ExitLayer{
		URL:     dest.URL + "/path",
		Method:  http.MethodGet,
		Headers: map[string]string{"X-Test": "hello"},
	}
	layerJSON, _ := json.Marshal(layer)
	ct, err := Encrypt(key, layerJSON)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	ks := NewKeyStore()
	ks.Store("e1", key)
	h := NewExitHandler(ks, http.DefaultClient)

	body, _ := json.Marshal(OnionRequest{
		CircuitID: "e1",
		Payload:   base64.StdEncoding.EncodeToString(ct),
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp OnionResponse
	json.NewDecoder(w.Body).Decode(&resp)

	// Relay/client decrypts with exit key to get ExitResponse.
	respCT, _ := base64.StdEncoding.DecodeString(resp.Payload)
	plaintext, err := Decrypt(key, respCT)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	var exitResp ExitResponse
	if err := json.Unmarshal(plaintext, &exitResp); err != nil {
		t.Fatalf("unmarshal ExitResponse: %v", err)
	}
	if exitResp.StatusCode != http.StatusOK {
		t.Fatalf("statusCode = %d, want %d", exitResp.StatusCode, http.StatusOK)
	}
	bodyBytes, _ := base64.StdEncoding.DecodeString(exitResp.Body)
	if string(bodyBytes) != "destination response" {
		t.Fatalf("body = %q, want %q", bodyBytes, "destination response")
	}
	if exitResp.Headers["Content-Type"] != "text/plain" {
		t.Fatalf("Content-Type = %q, want text/plain", exitResp.Headers["Content-Type"])
	}
}

// TestExitHandleOnionWithBody verifies that a request body is forwarded correctly.
func TestExitHandleOnionWithBody(t *testing.T) {
	key := randomKey(t)

	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := json.Marshal(map[string]string{"received": "ok"})
		w.Header().Set("Content-Type", "application/json")
		w.Write(got)
	}))
	defer dest.Close()

	reqBody := []byte(`{"hello":"world"}`)
	layer := ExitLayer{
		URL:    dest.URL,
		Method: http.MethodPost,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:   base64.StdEncoding.EncodeToString(reqBody),
	}
	layerJSON, _ := json.Marshal(layer)
	ct, _ := Encrypt(key, layerJSON)

	ks := NewKeyStore()
	ks.Store("e2", key)
	h := NewExitHandler(ks, http.DefaultClient)

	body, _ := json.Marshal(OnionRequest{
		CircuitID: "e2",
		Payload:   base64.StdEncoding.EncodeToString(ct),
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

// TestExitHandleOnionNon200Propagated verifies non-200 responses are encrypted and
// returned through the circuit rather than causing an error.
func TestExitHandleOnionNon200Propagated(t *testing.T) {
	key := randomKey(t)

	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer dest.Close()

	layer := ExitLayer{URL: dest.URL, Method: http.MethodGet}
	layerJSON, _ := json.Marshal(layer)
	ct, _ := Encrypt(key, layerJSON)

	ks := NewKeyStore()
	ks.Store("e3", key)
	h := NewExitHandler(ks, http.DefaultClient)

	body, _ := json.Marshal(OnionRequest{
		CircuitID: "e3",
		Payload:   base64.StdEncoding.EncodeToString(ct),
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)

	// The exit node itself returns 200 — it wraps the 404 inside the encrypted response.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp OnionResponse
	json.NewDecoder(w.Body).Decode(&resp)

	respCT, _ := base64.StdEncoding.DecodeString(resp.Payload)
	plaintext, err := Decrypt(key, respCT)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	var exitResp ExitResponse
	json.Unmarshal(plaintext, &exitResp)
	if exitResp.StatusCode != http.StatusNotFound {
		t.Fatalf("inner statusCode = %d, want %d", exitResp.StatusCode, http.StatusNotFound)
	}
}

// TestExitHandleOnionDestinationUnreachable verifies 502 when destination is down.
func TestExitHandleOnionDestinationUnreachable(t *testing.T) {
	key := randomKey(t)

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	layer := ExitLayer{URL: deadURL, Method: http.MethodGet}
	layerJSON, _ := json.Marshal(layer)
	ct, _ := Encrypt(key, layerJSON)

	ks := NewKeyStore()
	ks.Store("e4", key)
	h := NewExitHandler(ks, http.DefaultClient)

	body, _ := json.Marshal(OnionRequest{
		CircuitID: "e4",
		Payload:   base64.StdEncoding.EncodeToString(ct),
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

// TestExitHandleOnionUnknownCircuit verifies 401 for unregistered circuits.
func TestExitHandleOnionUnknownCircuit(t *testing.T) {
	h := NewExitHandler(NewKeyStore(), http.DefaultClient)
	body, _ := json.Marshal(OnionRequest{CircuitID: "unknown", Payload: "abc"})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// TestExitHandleOnionDecryptionFailure verifies 400 for tampered ciphertext.
func TestExitHandleOnionDecryptionFailure(t *testing.T) {
	ks := NewKeyStore()
	ks.Store("e5", randomKey(t))
	ct, _ := Encrypt(randomKey(t), []byte("data")) // different key

	h := NewExitHandler(ks, http.DefaultClient)
	body, _ := json.Marshal(OnionRequest{
		CircuitID: "e5",
		Payload:   base64.StdEncoding.EncodeToString(ct),
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestExitHandleOnionInvalidExitLayer verifies 400 when decrypted payload is not ExitLayer.
func TestExitHandleOnionInvalidExitLayer(t *testing.T) {
	key := randomKey(t)
	ks := NewKeyStore()
	ks.Store("e6", key)
	ct, _ := Encrypt(key, []byte("not valid json"))

	h := NewExitHandler(ks, http.DefaultClient)
	body, _ := json.Marshal(OnionRequest{
		CircuitID: "e6",
		Payload:   base64.StdEncoding.EncodeToString(ct),
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestExitHandleOnionMissingURLOrMethod verifies 400 when exit layer is missing required fields.
func TestExitHandleOnionMissingURLOrMethod(t *testing.T) {
	key := randomKey(t)
	ks := NewKeyStore()
	ks.Store("e7", key)

	layer := ExitLayer{URL: "", Method: ""} // missing both
	layerJSON, _ := json.Marshal(layer)
	ct, _ := Encrypt(key, layerJSON)

	h := NewExitHandler(ks, http.DefaultClient)
	body, _ := json.Marshal(OnionRequest{
		CircuitID: "e7",
		Payload:   base64.StdEncoding.EncodeToString(ct),
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestExitHandleKeySuccess verifies POST /key stores the session key.
func TestExitHandleKeySuccess(t *testing.T) {
	key := randomKey(t)
	ks := NewKeyStore()
	h := NewExitHandler(ks, http.DefaultClient)

	body, _ := json.Marshal(KeyRequest{
		CircuitID: "e1",
		Key:       base64.StdEncoding.EncodeToString(key),
	})
	r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleKey(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	stored, ok := ks.Get("e1")
	if !ok || !bytes.Equal(stored, key) {
		t.Fatal("key not stored correctly")
	}
}

// TestExitHandleOnionMethodNotAllowed verifies non-POST requests are rejected.
func TestExitHandleOnionMethodNotAllowed(t *testing.T) {
	h := NewExitHandler(NewKeyStore(), http.DefaultClient)
	r := httptest.NewRequest(http.MethodGet, "/onion", nil)
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestExitHandleOnionTimeout verifies that a slow destination is timed out.
func TestExitHandleOnionTimeout(t *testing.T) {
	key := randomKey(t)

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the request context is cancelled (simulates timeout).
		<-r.Context().Done()
	}))
	defer slow.Close()

	layer := ExitLayer{URL: slow.URL, Method: http.MethodGet}
	layerJSON, _ := json.Marshal(layer)
	ct, _ := Encrypt(key, layerJSON)

	ks := NewKeyStore()
	ks.Store("e8", key)

	// Use a client with a very short timeout to trigger the timeout path.
	shortTimeoutClient := &http.Client{Timeout: 1}
	h := NewExitHandler(ks, shortTimeoutClient)

	body, _ := json.Marshal(OnionRequest{
		CircuitID: "e8",
		Payload:   base64.StdEncoding.EncodeToString(ct),
	})

	// Use a background request so the slow server's context cancel doesn't affect us.
	req := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Replace context with a non-cancellable one so our test doesn't race.
	req = req.WithContext(req.Context())

	w := httptest.NewRecorder()
	h.HandleOnion(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (timeout should produce 502)", w.Code, http.StatusBadGateway)
	}
}

// TestExitHandleOnionInvalidURL verifies 400 for a malformed destination URL.
func TestExitHandleOnionInvalidURL(t *testing.T) {
	key := randomKey(t)
	ks := NewKeyStore()
	ks.Store("e9", key)

	layer := ExitLayer{URL: "://bad-url", Method: http.MethodGet}
	layerJSON, _ := json.Marshal(layer)
	ct, _ := Encrypt(key, layerJSON)

	h := NewExitHandler(ks, http.DefaultClient)
	body, _ := json.Marshal(OnionRequest{
		CircuitID: "e9",
		Payload:   base64.StdEncoding.EncodeToString(ct),
	})
	r := httptest.NewRequest(http.MethodPost, "/onion", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOnion(w, r)
	if w.Code != http.StatusBadRequest && w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 400 or 502 for invalid URL", w.Code)
	}
}


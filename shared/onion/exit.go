package onion

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
)

// ExitLayer is the plaintext structure in the innermost onion layer.
// It describes the HTTP request the exit node must execute.
type ExitLayer struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"` // plaintext request body
}

// ExitResponse is the HTTP response from the destination, returned encrypted
// through the circuit back to the client.
type ExitResponse struct {
	StatusCode int               `json:"statusCode"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       []byte            `json:"body,omitempty"` // plaintext response body
}

// ExitHandler provides POST /key and POST /onion for the exit node.
// Unlike the relay/guard Handler, it does not forward to another onion node —
// it decrypts the final layer and executes the plaintext HTTP request.
type ExitHandler struct {
	Keys   *KeyStore
	Client *http.Client
}

// NewExitHandler creates an ExitHandler with the given key store and HTTP client.
func NewExitHandler(keys *KeyStore, client *http.Client) *ExitHandler {
	return &ExitHandler{Keys: keys, Client: client}
}

// HandleKey registers an AES-256 session key for a circuit.
// POST /key  {"circuitId":"…","key":"<base64-32-bytes>"}
func (h *ExitHandler) HandleKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req KeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.CircuitID == "" || req.Key == "" {
		http.Error(w, "circuitId and key are required", http.StatusBadRequest)
		return
	}
	keyBytes, err := base64.StdEncoding.DecodeString(req.Key)
	if err != nil {
		http.Error(w, "key must be valid base64", http.StatusBadRequest)
		return
	}
	if len(keyBytes) != 32 {
		http.Error(w, "key must be 32 bytes for AES-256", http.StatusBadRequest)
		return
	}
	h.Keys.Store(req.CircuitID, keyBytes)
	log.Printf("[exit] session key stored for circuit %s", req.CircuitID)
	w.WriteHeader(http.StatusNoContent)
}

// HandleOnion decrypts the final onion layer, executes the plaintext HTTP request,
// encrypts the response with the session key, and returns it through the circuit.
// POST /onion  {"circuitId":"…","payload":"<ciphertext-bytes>"}
func (h *ExitHandler) HandleOnion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OnionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[exit] bad request: %v", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.CircuitID == "" || len(req.Payload) == 0 {
		http.Error(w, "circuitId and payload are required", http.StatusBadRequest)
		return
	}

	key, ok := h.Keys.Get(req.CircuitID)
	if !ok {
		log.Printf("[exit] unknown circuit %s", req.CircuitID)
		http.Error(w, "unknown circuit", http.StatusUnauthorized)
		return
	}

	plaintext, err := Decrypt(key, req.Payload)
	if err != nil {
		log.Printf("[exit] decryption failed for circuit %s: %v", req.CircuitID, err)
		http.Error(w, "decryption failed", http.StatusBadRequest)
		return
	}

	var layer ExitLayer
	if err := json.Unmarshal(plaintext, &layer); err != nil {
		log.Printf("[exit] invalid exit layer for circuit %s: %v", req.CircuitID, err)
		http.Error(w, "invalid exit layer format", http.StatusBadRequest)
		return
	}
	if layer.URL == "" || layer.Method == "" {
		http.Error(w, "exit layer must include url and method", http.StatusBadRequest)
		return
	}

	// Log the destination URL — never the client IP.
	log.Printf("[exit] circuit %s → %s %s", req.CircuitID, layer.Method, layer.URL)

	// Build the outbound request to the destination.
	var bodyReader io.Reader
	if len(layer.Body) > 0 {
		bodyReader = bytes.NewReader(layer.Body)
	}

	destReq, err := http.NewRequestWithContext(r.Context(), layer.Method, layer.URL, bodyReader)
	if err != nil {
		log.Printf("[exit] build destination request for circuit %s: %v", req.CircuitID, err)
		http.Error(w, "invalid destination request", http.StatusBadRequest)
		return
	}
	for k, v := range layer.Headers {
		destReq.Header.Set(k, v)
	}

	destResp, err := h.Client.Do(destReq)
	if err != nil {
		log.Printf("[exit] circuit %s destination %s unreachable: %v", req.CircuitID, layer.URL, err)
		http.Error(w, "destination unreachable", http.StatusBadGateway)
		return
	}
	defer destResp.Body.Close()

	respBody, err := io.ReadAll(destResp.Body)
	if err != nil {
		log.Printf("[exit] read destination response for circuit %s: %v", req.CircuitID, err)
		http.Error(w, "failed to read destination response", http.StatusBadGateway)
		return
	}

	// Collect response headers (first value per key).
	respHeaders := make(map[string]string, len(destResp.Header))
	for k, vals := range destResp.Header {
		if len(vals) > 0 {
			respHeaders[k] = vals[0]
		}
	}

	exitResp := ExitResponse{
		StatusCode: destResp.StatusCode,
		Headers:    respHeaders,
		Body:       respBody,
	}
	exitRespJSON, err := json.Marshal(exitResp)
	if err != nil {
		log.Printf("[exit] marshal exit response for circuit %s: %v", req.CircuitID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	encrypted, err := Encrypt(key, exitRespJSON)
	if err != nil {
		log.Printf("[exit] encrypt response for circuit %s: %v", req.CircuitID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(OnionResponse{
		Payload: encrypted,
	})
}

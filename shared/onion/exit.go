package onion

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
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

// UnwrapExitLayer decrypts the innermost onion layer with AES-256-GCM and
// decodes the plaintext HTTP request that the exit node must execute.
func UnwrapExitLayer(key, ciphertext []byte) (*ExitLayer, error) {
	plaintext, err := Decrypt(key, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt exit layer: %w", err)
	}

	var layer ExitLayer
	if err := json.Unmarshal(plaintext, &layer); err != nil {
		return nil, fmt.Errorf("decode exit layer: %w", err)
	}
	if layer.URL == "" || layer.Method == "" {
		return nil, errors.New("exit layer must include url and method")
	}

	return &layer, nil
}

// ExecuteExitPayload decrypts the final onion layer, executes the destination
// request, and returns the encrypted response payload for the return path.
func ExecuteExitPayload(ctx context.Context, client *http.Client, key, payload []byte) ([]byte, *ExitLayer, error) {
	layer, err := UnwrapExitLayer(key, payload)
	if err != nil {
		return nil, nil, err
	}

	exitResp, err := executeExitRequest(ctx, client, layer)
	if err != nil {
		return nil, layer, err
	}

	encrypted, err := encryptExitResponse(key, exitResp)
	if err != nil {
		return nil, layer, err
	}

	return encrypted, layer, nil
}

func executeExitRequest(ctx context.Context, client *http.Client, layer *ExitLayer) (*ExitResponse, error) {
	// Build the outbound request to the destination.
	var bodyReader io.Reader
	if len(layer.Body) > 0 {
		bodyReader = bytes.NewReader(layer.Body)
	}

	destReq, err := http.NewRequestWithContext(ctx, layer.Method, layer.URL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("invalid destination request: %w", err)
	}
	for k, v := range layer.Headers {
		destReq.Header.Set(k, v)
	}

	destResp, err := client.Do(destReq)
	if err != nil {
		return nil, fmt.Errorf("destination unreachable: %w", err)
	}
	defer destResp.Body.Close()

	respBody, err := io.ReadAll(destResp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read destination response: %w", err)
	}

	respHeaders := make(map[string]string, len(destResp.Header))
	for k, vals := range destResp.Header {
		if len(vals) > 0 {
			respHeaders[k] = vals[0]
		}
	}

	return &ExitResponse{
		StatusCode: destResp.StatusCode,
		Headers:    respHeaders,
		Body:       respBody,
	}, nil
}

func encryptExitResponse(key []byte, exitResp *ExitResponse) ([]byte, error) {
	exitRespJSON, err := json.Marshal(exitResp)
	if err != nil {
		return nil, fmt.Errorf("internal error: marshal exit response: %w", err)
	}

	encrypted, err := Encrypt(key, exitRespJSON)
	if err != nil {
		return nil, fmt.Errorf("internal error: encrypt response: %w", err)
	}

	return encrypted, nil
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

	encrypted, layer, err := ExecuteExitPayload(r.Context(), h.Client, key, req.Payload)
	if err != nil {
		log.Printf("[exit] circuit %s failed: %v", req.CircuitID, err)
		switch {
		case strings.Contains(err.Error(), "invalid destination request"), strings.Contains(err.Error(), "exit layer"):
			http.Error(w, "invalid exit layer format", http.StatusBadRequest)
		case strings.Contains(err.Error(), "destination unreachable"), strings.Contains(err.Error(), "failed to read destination response"):
			http.Error(w, "destination unreachable", http.StatusBadGateway)
		default:
			http.Error(w, "decryption failed", http.StatusBadRequest)
		}
		return
	}

	// Log the destination URL — never the client IP.
	log.Printf("[exit] circuit %s → %s %s", req.CircuitID, layer.Method, layer.URL)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(OnionResponse{
		Payload: encrypted,
	})
}

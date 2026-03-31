package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
)

// OnionRequest is the JSON body sent to the /onion endpoint.
type OnionRequest struct {
	CircuitID string `json:"circuitId"`
	Payload   string `json:"payload"` // base64-encoded AES-256-GCM ciphertext
}

// OnionResponse is the JSON body returned from the /onion endpoint.
type OnionResponse struct {
	Payload string `json:"payload"` // base64-encoded AES-256-GCM ciphertext
}

// Layer is the plaintext structure revealed after decrypting one onion layer.
type Layer struct {
	NextHop string `json:"nextHop"` // host:port of the next node
	Payload string `json:"payload"` // base64-encoded inner ciphertext for the next hop
}

// KeyRequest is the JSON body sent to the /key endpoint.
type KeyRequest struct {
	CircuitID string `json:"circuitId"`
	Key       string `json:"key"` // base64-encoded 32-byte AES-256 session key
}

// keyStore holds per-circuit AES-256 session keys.
type keyStore struct {
	mu   sync.RWMutex
	keys map[string][]byte
}

func newKeyStore() *keyStore {
	return &keyStore{keys: make(map[string][]byte)}
}

func (s *keyStore) store(circuitID string, key []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(key))
	copy(cp, key)
	s.keys[circuitID] = cp
}

func (s *keyStore) get(circuitID string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.keys[circuitID]
	return key, ok
}

// encrypt encrypts plaintext with AES-256-GCM.
// The 12-byte random nonce is prepended to the returned ciphertext.
func encrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt decrypts AES-256-GCM ciphertext where the nonce is prepended.
func decrypt(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, data := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, data, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}

// onionHandler implements the guard node's onion routing endpoints.
type onionHandler struct {
	keys   *keyStore
	client *http.Client
}

func newOnionHandler(keys *keyStore, client *http.Client) *onionHandler {
	return &onionHandler{keys: keys, client: client}
}

// handleKey registers an AES-256 session key for a circuit.
// POST /key  {"circuitId":"…","key":"<base64-32-bytes>"}
func (h *onionHandler) handleKey(w http.ResponseWriter, r *http.Request) {
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
	h.keys.store(req.CircuitID, keyBytes)
	log.Printf("[guard] session key stored for circuit %s", req.CircuitID)
	w.WriteHeader(http.StatusNoContent)
}

// handleOnion decrypts the outermost onion layer, forwards the inner payload
// to the next hop, and re-encrypts the response before returning it to the caller.
// POST /onion  {"circuitId":"…","payload":"<base64-ciphertext>"}
func (h *onionHandler) handleOnion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clientIP := r.RemoteAddr

	var req OnionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[guard] bad request from %s: %v", clientIP, err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.CircuitID == "" || req.Payload == "" {
		log.Printf("[guard] bad request from %s: missing circuitId or payload", clientIP)
		http.Error(w, "circuitId and payload are required", http.StatusBadRequest)
		return
	}

	key, ok := h.keys.get(req.CircuitID)
	if !ok {
		log.Printf("[guard] unknown circuit %s from %s", req.CircuitID, clientIP)
		http.Error(w, "unknown circuit", http.StatusUnauthorized)
		return
	}

	ciphertext, err := base64.StdEncoding.DecodeString(req.Payload)
	if err != nil {
		log.Printf("[guard] invalid payload encoding from %s: %v", clientIP, err)
		http.Error(w, "payload must be valid base64", http.StatusBadRequest)
		return
	}

	plaintext, err := decrypt(key, ciphertext)
	if err != nil {
		log.Printf("[guard] decryption failed for circuit %s from %s: %v", req.CircuitID, clientIP, err)
		http.Error(w, "decryption failed", http.StatusBadRequest)
		return
	}

	var layer Layer
	if err := json.Unmarshal(plaintext, &layer); err != nil {
		log.Printf("[guard] invalid layer format from %s: %v", clientIP, err)
		http.Error(w, "invalid layer format", http.StatusBadRequest)
		return
	}

	// Log client IP and next hop — never the final destination.
	log.Printf("[guard] circuit %s: %s → %s", req.CircuitID, clientIP, layer.NextHop)

	// Forward inner payload to the relay node.
	fwdBody, err := json.Marshal(OnionRequest{CircuitID: req.CircuitID, Payload: layer.Payload})
	if err != nil {
		log.Printf("[guard] marshal forward request: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nextHopURL := "http://" + layer.NextHop + "/onion"
	resp, err := h.client.Post(nextHopURL, "application/json", bytes.NewReader(fwdBody))
	if err != nil {
		log.Printf("[guard] forward to %s failed: %v", layer.NextHop, err)
		http.Error(w, "next hop unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[guard] next hop %s returned %d: %s", layer.NextHop, resp.StatusCode, body)
		http.Error(w, "next hop error", http.StatusBadGateway)
		return
	}

	var relayResp OnionResponse
	if err := json.NewDecoder(resp.Body).Decode(&relayResp); err != nil {
		log.Printf("[guard] invalid relay response from %s: %v", layer.NextHop, err)
		http.Error(w, "invalid relay response", http.StatusBadGateway)
		return
	}

	relayPayload, err := base64.StdEncoding.DecodeString(relayResp.Payload)
	if err != nil {
		log.Printf("[guard] relay payload base64 error: %v", err)
		http.Error(w, "invalid relay response payload", http.StatusBadGateway)
		return
	}

	// Return path: wrap the relay's response in one more encryption layer with our session key.
	encrypted, err := encrypt(key, relayPayload)
	if err != nil {
		log.Printf("[guard] encrypt response: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(OnionResponse{
		Payload: base64.StdEncoding.EncodeToString(encrypted),
	})
}

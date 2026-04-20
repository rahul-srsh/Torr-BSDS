// Package onion implements the HopVault onion routing crypto and forwarding
// layer: AES-256-GCM layer encryption, RSA-OAEP session-key transport, the
// per-circuit KeyStore, and the /key /setup /onion HTTP handlers used by guard,
// relay, and exit nodes.
package onion

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

// OnionRequest is the JSON body for POST /onion.
type OnionRequest struct {
	CircuitID string `json:"circuitId"`
	Payload   []byte `json:"payload"` // raw AES-256-GCM ciphertext
}

// OnionResponse is the JSON body returned from POST /onion.
type OnionResponse struct {
	Payload []byte `json:"payload"` // raw AES-256-GCM ciphertext
}

// Layer is the plaintext structure revealed after decrypting one onion layer.
type Layer struct {
	NextHop string `json:"nextHop"` // host:port of the next node
	Payload []byte `json:"payload"` // inner ciphertext for the next hop
}

// KeyRequest is the JSON body for POST /key.
type KeyRequest struct {
	CircuitID string `json:"circuitId"`
	Key       string `json:"key"` // base64-encoded 32-byte AES-256 session key
}

// KeyStore holds per-circuit AES-256 session keys.
type KeyStore struct {
	mu   sync.RWMutex
	keys map[string][]byte
}

// NewKeyStore creates an empty KeyStore.
func NewKeyStore() *KeyStore {
	return &KeyStore{keys: make(map[string][]byte)}
}

// Store saves an independent copy of key for the given circuit.
func (s *KeyStore) Store(circuitID string, key []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(key))
	copy(cp, key)
	s.keys[circuitID] = cp
}

// Get retrieves the session key for a circuit.
func (s *KeyStore) Get(circuitID string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.keys[circuitID]
	return key, ok
}

// Encrypt encrypts plaintext with AES-256-GCM.
// The 12-byte random nonce is prepended to the returned ciphertext.
func Encrypt(key, plaintext []byte) ([]byte, error) {
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

// Decrypt decrypts AES-256-GCM ciphertext where the nonce is prepended.
func Decrypt(key, ciphertext []byte) ([]byte, error) {
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

// UnwrapLayer decrypts one onion hop with AES-256-GCM and decodes the next-hop
// routing metadata plus the still-encrypted inner payload.
func UnwrapLayer(key, ciphertext []byte) (*Layer, error) {
	plaintext, err := Decrypt(key, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt onion layer: %w", err)
	}

	var layer Layer
	if err := json.Unmarshal(plaintext, &layer); err != nil {
		return nil, fmt.Errorf("decode onion layer: %w", err)
	}
	if layer.NextHop == "" || len(layer.Payload) == 0 {
		return nil, errors.New("layer must include nextHop and payload")
	}

	return &layer, nil
}

// Handler provides POST /key and POST /onion endpoints for any onion routing node.
type Handler struct {
	Keys            *KeyStore
	Client          *http.Client
	NodeLabel       string // "guard", "relay", or "exit" — used in log messages
	AllowDirectExit bool
}

// NewHandler creates a Handler for the named node type.
func NewHandler(keys *KeyStore, client *http.Client, nodeLabel string) *Handler {
	return &Handler{Keys: keys, Client: client, NodeLabel: nodeLabel}
}

// NewHandlerWithDirectExit creates a forwarding handler that can also terminate
// a single-hop circuit by acting as the exit node after decrypting the guard layer.
func NewHandlerWithDirectExit(keys *KeyStore, client *http.Client, nodeLabel string) *Handler {
	return &Handler{
		Keys:            keys,
		Client:          client,
		NodeLabel:       nodeLabel,
		AllowDirectExit: true,
	}
}

// HandleKey registers an AES-256 session key for a circuit.
// POST /key  {"circuitId":"…","key":"<base64-32-bytes>"}
func (h *Handler) HandleKey(w http.ResponseWriter, r *http.Request) {
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
	log.Printf("[%s] session key stored for circuit %s", h.NodeLabel, req.CircuitID)
	w.WriteHeader(http.StatusNoContent)
}

// HandleOnion decrypts the outermost onion layer, forwards the inner payload to the
// next hop, and re-encrypts the response before returning it to the caller.
// POST /onion  {"circuitId":"…","payload":"<ciphertext-bytes>"}
func (h *Handler) HandleOnion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	prevHop := r.RemoteAddr

	var req OnionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[%s] bad request from %s: %v", h.NodeLabel, prevHop, err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.CircuitID == "" || len(req.Payload) == 0 {
		log.Printf("[%s] bad request from %s: missing circuitId or payload", h.NodeLabel, prevHop)
		http.Error(w, "circuitId and payload are required", http.StatusBadRequest)
		return
	}

	key, ok := h.Keys.Get(req.CircuitID)
	if !ok {
		log.Printf("[%s] unknown circuit %s from %s", h.NodeLabel, req.CircuitID, prevHop)
		http.Error(w, "unknown circuit", http.StatusUnauthorized)
		return
	}

	layer, err := UnwrapLayer(key, req.Payload)
	if err != nil {
		if h.AllowDirectExit {
			encrypted, exitLayer, exitErr := ExecuteExitPayload(r.Context(), h.Client, key, req.Payload)
			if exitErr == nil {
				log.Printf("[%s] circuit %s: %s → direct %s %s", h.NodeLabel, req.CircuitID, prevHop, exitLayer.Method, exitLayer.URL)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(OnionResponse{Payload: encrypted})
				return
			}
		}

		log.Printf("[%s] decryption failed for circuit %s from %s: %v", h.NodeLabel, req.CircuitID, prevHop, err)
		http.Error(w, "decryption failed", http.StatusBadRequest)
		return
	}

	// Log only the previous hop and next hop — never the original client or final destination.
	log.Printf("[%s] circuit %s: %s → %s", h.NodeLabel, req.CircuitID, prevHop, layer.NextHop)

	// Forward inner payload to the next node.
	fwdBody, err := json.Marshal(OnionRequest{CircuitID: req.CircuitID, Payload: layer.Payload})
	if err != nil {
		log.Printf("[%s] marshal forward request: %v", h.NodeLabel, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nextHopURL := "http://" + layer.NextHop + "/onion"
	resp, err := h.Client.Post(nextHopURL, "application/json", bytes.NewReader(fwdBody))
	if err != nil {
		log.Printf("[%s] forward to %s failed: %v", h.NodeLabel, layer.NextHop, err)
		http.Error(w, "next hop unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[%s] next hop %s returned %d: %s", h.NodeLabel, layer.NextHop, resp.StatusCode, body)
		http.Error(w, "next hop error", http.StatusBadGateway)
		return
	}

	var nextResp OnionResponse
	if err := json.NewDecoder(resp.Body).Decode(&nextResp); err != nil {
		log.Printf("[%s] invalid response from %s: %v", h.NodeLabel, layer.NextHop, err)
		http.Error(w, "invalid next hop response", http.StatusBadGateway)
		return
	}

	// Return path: wrap the next hop's response in one more encryption layer with our session key.
	encrypted, err := Encrypt(key, nextResp.Payload)
	if err != nil {
		log.Printf("[%s] encrypt response: %v", h.NodeLabel, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(OnionResponse{
		Payload: encrypted,
	})
}

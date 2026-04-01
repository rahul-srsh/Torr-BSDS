package onion

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
)

// KeySetupRequest is the JSON body for POST /setup.
type KeySetupRequest struct {
	CircuitID    string `json:"circuitId"`
	EncryptedKey string `json:"encryptedKey"` // base64(RSA-OAEP-SHA256 encrypted 32-byte AES key)
}

// EncryptKey encrypts a 32-byte AES-256 session key with an RSA public key
// using OAEP with SHA-256. Returns the raw ciphertext (not base64).
// No plaintext key is logged.
func EncryptKey(pub *rsa.PublicKey, aesKey []byte) ([]byte, error) {
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, aesKey, nil)
	if err != nil {
		return nil, fmt.Errorf("RSA-OAEP encrypt: %w", err)
	}
	return ct, nil
}

// DecryptKey decrypts an RSA-OAEP-SHA256 ciphertext to recover the AES session key.
// No plaintext key is logged.
func DecryptKey(priv *rsa.PrivateKey, encrypted []byte) ([]byte, error) {
	pt, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, priv, encrypted, nil)
	if err != nil {
		return nil, fmt.Errorf("RSA-OAEP decrypt: %w", err)
	}
	return pt, nil
}

// ParsePublicKey parses a PKIX PEM-encoded RSA public key string.
func ParsePublicKey(pemStr string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKIX public key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key is not RSA")
	}
	return rsaPub, nil
}

// HandleSetup returns an HTTP handler for POST /setup — the key exchange
// endpoint used during circuit setup. The client sends an RSA-OAEP-SHA256
// encrypted AES-256 session key; the node decrypts it with its private key
// and stores it in the KeyStore for the given circuit.
// No plaintext key is ever logged.
func HandleSetup(keys *KeyStore, priv *rsa.PrivateKey) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req KeySetupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if req.CircuitID == "" || req.EncryptedKey == "" {
			http.Error(w, "circuitId and encryptedKey are required", http.StatusBadRequest)
			return
		}

		encrypted, err := base64.StdEncoding.DecodeString(req.EncryptedKey)
		if err != nil {
			http.Error(w, "encryptedKey must be valid base64", http.StatusBadRequest)
			return
		}

		aesKey, err := DecryptKey(priv, encrypted)
		if err != nil {
			// Log circuit ID only — never log key material.
			log.Printf("[setup] key decryption failed for circuit %s", req.CircuitID)
			http.Error(w, "key decryption failed", http.StatusBadRequest)
			return
		}

		if len(aesKey) != 32 {
			log.Printf("[setup] wrong key length %d for circuit %s", len(aesKey), req.CircuitID)
			http.Error(w, "decrypted key must be 32 bytes (AES-256)", http.StatusBadRequest)
			return
		}

		keys.Store(req.CircuitID, aesKey)
		log.Printf("[setup] session key established for circuit %s", req.CircuitID)
		w.WriteHeader(http.StatusNoContent)
	}
}

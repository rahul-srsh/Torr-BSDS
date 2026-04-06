package onion

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testRSAKeyPair generates a 2048-bit RSA key pair for tests.
func testRSAKeyPair(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes}))
	return priv, pubPEM
}

// ---- EncryptKey / DecryptKey ----

func TestEncryptDecryptKeyRoundTrip(t *testing.T) {
	priv, _ := testRSAKeyPair(t)
	aesKey := randomKey(t)

	ct, err := EncryptKey(&priv.PublicKey, aesKey)
	if err != nil {
		t.Fatalf("EncryptKey: %v", err)
	}
	got, err := DecryptKey(priv, ct)
	if err != nil {
		t.Fatalf("DecryptKey: %v", err)
	}
	if !bytes.Equal(got, aesKey) {
		t.Fatal("decrypted key does not match original")
	}
}

func TestEncryptKeyProducesDistinctCiphertexts(t *testing.T) {
	priv, _ := testRSAKeyPair(t)
	key := randomKey(t)
	ct1, _ := EncryptKey(&priv.PublicKey, key)
	ct2, _ := EncryptKey(&priv.PublicKey, key)
	if bytes.Equal(ct1, ct2) {
		t.Fatal("two OAEP encryptions of the same key must differ (random padding)")
	}
}

func TestEncryptKeyRejectsOversizedPlaintext(t *testing.T) {
	priv, _ := testRSAKeyPair(t)
	oversized := make([]byte, 512)
	_, err := EncryptKey(&priv.PublicKey, oversized)
	if err == nil {
		t.Fatal("expected oversized plaintext to be rejected")
	}
}

func TestDecryptKeyWrongPrivateKey(t *testing.T) {
	priv1, _ := testRSAKeyPair(t)
	priv2, _ := testRSAKeyPair(t)
	ct, _ := EncryptKey(&priv1.PublicKey, randomKey(t))
	_, err := DecryptKey(priv2, ct)
	if err == nil {
		t.Fatal("expected error when decrypting with wrong private key")
	}
}

func TestDecryptKeyTamperedCiphertext(t *testing.T) {
	priv, _ := testRSAKeyPair(t)
	ct, _ := EncryptKey(&priv.PublicKey, randomKey(t))
	ct[0] ^= 0xFF
	_, err := DecryptKey(priv, ct)
	if err == nil {
		t.Fatal("expected error for tampered ciphertext")
	}
}

// ---- ParsePublicKey ----

func TestParsePublicKeyRoundTrip(t *testing.T) {
	priv, pubPEM := testRSAKeyPair(t)
	pub, err := ParsePublicKey(pubPEM)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	if pub.N.Cmp(priv.PublicKey.N) != 0 {
		t.Fatal("parsed public key does not match original")
	}
}

func TestParsePublicKeyInvalidPEM(t *testing.T) {
	_, err := ParsePublicKey("not a pem block")
	if err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

func TestParsePublicKeyEmptyString(t *testing.T) {
	_, err := ParsePublicKey("")
	if err == nil {
		t.Fatal("expected error for empty string")
	}
}

// ---- HandleSetup ----

func TestHandleSetupSuccess(t *testing.T) {
	priv, _ := testRSAKeyPair(t)
	aesKey := randomKey(t)
	ct, _ := EncryptKey(&priv.PublicKey, aesKey)

	ks := NewKeyStore()
	handler := HandleSetup(ks, priv)

	body, _ := json.Marshal(KeySetupRequest{
		CircuitID:    "setup-1",
		EncryptedKey: base64.StdEncoding.EncodeToString(ct),
	})
	r := httptest.NewRequest(http.MethodPost, "/setup", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusNoContent, w.Body.String())
	}
	stored, ok := ks.Get("setup-1")
	if !ok {
		t.Fatal("key not stored after /setup")
	}
	if !bytes.Equal(stored, aesKey) {
		t.Fatal("stored key does not match original AES key")
	}
}

func TestHandleSetupMethodNotAllowed(t *testing.T) {
	priv, _ := testRSAKeyPair(t)
	r := httptest.NewRequest(http.MethodGet, "/setup", nil)
	w := httptest.NewRecorder()
	HandleSetup(NewKeyStore(), priv)(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleSetupInvalidJSON(t *testing.T) {
	priv, _ := testRSAKeyPair(t)
	r := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader("{bad"))
	w := httptest.NewRecorder()
	HandleSetup(NewKeyStore(), priv)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSetupMissingFields(t *testing.T) {
	priv, _ := testRSAKeyPair(t)
	for _, req := range []KeySetupRequest{
		{EncryptedKey: "abc"}, // missing circuitId
		{CircuitID: "c1"},     // missing encryptedKey
	} {
		body, _ := json.Marshal(req)
		r := httptest.NewRequest(http.MethodPost, "/setup", bytes.NewReader(body))
		w := httptest.NewRecorder()
		HandleSetup(NewKeyStore(), priv)(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	}
}

func TestHandleSetupInvalidBase64(t *testing.T) {
	priv, _ := testRSAKeyPair(t)
	body, _ := json.Marshal(KeySetupRequest{CircuitID: "c1", EncryptedKey: "!!!not-base64!!!"})
	r := httptest.NewRequest(http.MethodPost, "/setup", bytes.NewReader(body))
	w := httptest.NewRecorder()
	HandleSetup(NewKeyStore(), priv)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSetupDecryptFailure(t *testing.T) {
	priv, _ := testRSAKeyPair(t)
	_, wrongPub := testRSAKeyPair(t)
	pub, _ := ParsePublicKey(wrongPub)
	ct, _ := EncryptKey(pub, randomKey(t)) // encrypted for wrong key

	body, _ := json.Marshal(KeySetupRequest{
		CircuitID:    "c1",
		EncryptedKey: base64.StdEncoding.EncodeToString(ct),
	})
	r := httptest.NewRequest(http.MethodPost, "/setup", bytes.NewReader(body))
	w := httptest.NewRecorder()
	HandleSetup(NewKeyStore(), priv)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSetupTwoCircuitsHaveIndependentKeys(t *testing.T) {
	priv, _ := testRSAKeyPair(t)
	ks := NewKeyStore()
	handler := HandleSetup(ks, priv)

	keyA := randomKey(t)
	keyB := randomKey(t)

	for _, tc := range []struct {
		id  string
		key []byte
	}{
		{"circuit-A", keyA},
		{"circuit-B", keyB},
	} {
		ct, _ := EncryptKey(&priv.PublicKey, tc.key)
		body, _ := json.Marshal(KeySetupRequest{
			CircuitID:    tc.id,
			EncryptedKey: base64.StdEncoding.EncodeToString(ct),
		})
		r := httptest.NewRequest(http.MethodPost, "/setup", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("%s: status = %d: %s", tc.id, w.Code, w.Body.String())
		}
	}

	a, _ := ks.Get("circuit-A")
	b, _ := ks.Get("circuit-B")
	if bytes.Equal(a, b) {
		t.Fatal("two circuits must have independent session keys")
	}
	if !bytes.Equal(a, keyA) || !bytes.Equal(b, keyB) {
		t.Fatal("stored keys do not match originals")
	}
}

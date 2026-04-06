package onion

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type privacyFixture struct {
	clientIP       string
	relayAddr      string
	exitAddr       string
	destinationURL string
	requestBody    []byte
	guardKey       []byte
	relayKey       []byte
	exitKey        []byte
	guardCipher    []byte
	relayCipher    []byte
	exitCipher     []byte
}

func buildPrivacyFixture(t *testing.T) *privacyFixture {
	t.Helper()

	fx := &privacyFixture{
		clientIP:       "198.51.100.77:54321",
		relayAddr:      "10.0.0.2:8082",
		exitAddr:       "10.0.0.3:8083",
		destinationURL: "https://example.com/private/resource",
		requestBody:    []byte(`{"token":"top-secret"}`),
		guardKey:       randomKey(t),
		relayKey:       randomKey(t),
		exitKey:        randomKey(t),
	}

	exitLayerJSON, err := json.Marshal(ExitLayer{
		URL:     fx.destinationURL,
		Method:  http.MethodPost,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    fx.requestBody,
	})
	if err != nil {
		t.Fatalf("marshal exit layer: %v", err)
	}
	fx.exitCipher, err = Encrypt(fx.exitKey, exitLayerJSON)
	if err != nil {
		t.Fatalf("encrypt exit layer: %v", err)
	}

	relayLayerJSON, err := json.Marshal(Layer{
		NextHop: fx.exitAddr,
		Payload: fx.exitCipher,
	})
	if err != nil {
		t.Fatalf("marshal relay layer: %v", err)
	}
	fx.relayCipher, err = Encrypt(fx.relayKey, relayLayerJSON)
	if err != nil {
		t.Fatalf("encrypt relay layer: %v", err)
	}

	guardLayerJSON, err := json.Marshal(Layer{
		NextHop: fx.relayAddr,
		Payload: fx.relayCipher,
	})
	if err != nil {
		t.Fatalf("marshal guard layer: %v", err)
	}
	fx.guardCipher, err = Encrypt(fx.guardKey, guardLayerJSON)
	if err != nil {
		t.Fatalf("encrypt guard layer: %v", err)
	}

	return fx
}

func TestPrivacyBoundariesAcrossCircuit(t *testing.T) {
	for i := 0; i < 10; i++ {
		fx := buildPrivacyFixture(t)

		guardPlain, err := Decrypt(fx.guardKey, fx.guardCipher)
		if err != nil {
			t.Fatalf("decrypt guard layer: %v", err)
		}
		if !bytes.Contains(guardPlain, []byte(fx.relayAddr)) {
			t.Fatal("guard layer should include the relay address")
		}
		if bytes.Contains(guardPlain, []byte(fx.destinationURL)) {
			t.Fatal("guard layer must not expose the destination URL")
		}

		guardLayer, err := UnwrapLayer(fx.guardKey, fx.guardCipher)
		if err != nil {
			t.Fatalf("unwrap guard layer: %v", err)
		}

		relayPlain, err := Decrypt(fx.relayKey, guardLayer.Payload)
		if err != nil {
			t.Fatalf("decrypt relay layer: %v", err)
		}
		if !bytes.Contains(relayPlain, []byte(fx.exitAddr)) {
			t.Fatal("relay layer should include the exit address")
		}
		if bytes.Contains(relayPlain, []byte(fx.clientIP)) {
			t.Fatal("relay layer must not expose the client IP")
		}
		if bytes.Contains(relayPlain, []byte(fx.destinationURL)) {
			t.Fatal("relay layer must not expose the destination URL")
		}

		relayLayer, err := UnwrapLayer(fx.relayKey, guardLayer.Payload)
		if err != nil {
			t.Fatalf("unwrap relay layer: %v", err)
		}

		exitPlain, err := Decrypt(fx.exitKey, relayLayer.Payload)
		if err != nil {
			t.Fatalf("decrypt exit layer: %v", err)
		}
		if !bytes.Contains(exitPlain, []byte(fx.destinationURL)) {
			t.Fatal("exit layer should include the destination URL")
		}
		if bytes.Contains(exitPlain, []byte(fx.clientIP)) {
			t.Fatal("exit layer must not expose the client IP")
		}
	}
}

func TestWrongKeyAtAnyHopFails(t *testing.T) {
	fx := buildPrivacyFixture(t)

	if _, err := UnwrapLayer(randomKey(t), fx.guardCipher); err == nil {
		t.Fatal("guard layer should fail with the wrong key")
	}

	guardLayer, err := UnwrapLayer(fx.guardKey, fx.guardCipher)
	if err != nil {
		t.Fatalf("unwrap guard layer: %v", err)
	}
	if _, err := UnwrapLayer(randomKey(t), guardLayer.Payload); err == nil {
		t.Fatal("relay layer should fail with the wrong key")
	}

	relayLayer, err := UnwrapLayer(fx.relayKey, guardLayer.Payload)
	if err != nil {
		t.Fatalf("unwrap relay layer: %v", err)
	}
	if _, err := UnwrapExitLayer(randomKey(t), relayLayer.Payload); err == nil {
		t.Fatal("exit layer should fail with the wrong key")
	}
}

func TestReorderedNodesCannotUnwrap(t *testing.T) {
	fx := buildPrivacyFixture(t)

	if _, err := UnwrapLayer(fx.relayKey, fx.guardCipher); err == nil {
		t.Fatal("relay should not be able to unwrap the guard layer directly")
	}

	guardLayer, err := UnwrapLayer(fx.guardKey, fx.guardCipher)
	if err != nil {
		t.Fatalf("unwrap guard layer: %v", err)
	}
	if _, err := UnwrapExitLayer(fx.exitKey, guardLayer.Payload); err == nil {
		t.Fatal("exit should not be able to unwrap the relay layer directly")
	}
}

func TestUnwrapLayerInvalidJSON(t *testing.T) {
	key := randomKey(t)
	ciphertext, err := Encrypt(key, []byte("not-json"))
	if err != nil {
		t.Fatalf("encrypt invalid payload: %v", err)
	}

	_, err = UnwrapLayer(key, ciphertext)
	if err == nil {
		t.Fatal("expected invalid JSON to fail")
	}
	if !strings.Contains(err.Error(), "decode onion layer") {
		t.Fatalf("error = %q, want decode onion layer context", err)
	}
}

func TestUnwrapLayerMissingFields(t *testing.T) {
	key := randomKey(t)
	plaintext, err := json.Marshal(Layer{NextHop: "", Payload: nil})
	if err != nil {
		t.Fatalf("marshal layer: %v", err)
	}
	ciphertext, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("encrypt layer: %v", err)
	}

	_, err = UnwrapLayer(key, ciphertext)
	if err == nil {
		t.Fatal("expected missing fields to fail")
	}
}

func TestUnwrapExitLayerWrongKeyFails(t *testing.T) {
	fx := buildPrivacyFixture(t)
	if _, err := UnwrapExitLayer(randomKey(t), fx.exitCipher); err == nil {
		t.Fatal("expected wrong key to fail for exit layer")
	}
}

func TestUnwrapExitLayerInvalidJSON(t *testing.T) {
	key := randomKey(t)
	ciphertext, err := Encrypt(key, []byte("not-json"))
	if err != nil {
		t.Fatalf("encrypt invalid exit layer: %v", err)
	}

	_, err = UnwrapExitLayer(key, ciphertext)
	if err == nil {
		t.Fatal("expected invalid exit JSON to fail")
	}
	if !strings.Contains(err.Error(), "decode exit layer") {
		t.Fatalf("error = %q, want decode exit layer context", err)
	}
}

func TestUnwrapExitLayerMissingFields(t *testing.T) {
	key := randomKey(t)
	plaintext, err := json.Marshal(ExitLayer{})
	if err != nil {
		t.Fatalf("marshal exit layer: %v", err)
	}
	ciphertext, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("encrypt exit layer: %v", err)
	}

	_, err = UnwrapExitLayer(key, ciphertext)
	if err == nil {
		t.Fatal("expected missing exit fields to fail")
	}
}

func TestExitHandleKeyValidationBranches(t *testing.T) {
	h := NewExitHandler(NewKeyStore(), http.DefaultClient)

	t.Run("method not allowed", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/key", nil)
		w := httptest.NewRecorder()
		h.HandleKey(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/key", strings.NewReader("{bad"))
		w := httptest.NewRecorder()
		h.HandleKey(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("missing fields", func(t *testing.T) {
		body, _ := json.Marshal(KeyRequest{CircuitID: "c1"})
		r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
		w := httptest.NewRecorder()
		h.HandleKey(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid base64", func(t *testing.T) {
		body, _ := json.Marshal(KeyRequest{CircuitID: "c1", Key: "not-base64"})
		r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
		w := httptest.NewRecorder()
		h.HandleKey(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("wrong length", func(t *testing.T) {
		body, _ := json.Marshal(KeyRequest{CircuitID: "c1", Key: "YWJj"})
		r := httptest.NewRequest(http.MethodPost, "/key", bytes.NewReader(body))
		w := httptest.NewRecorder()
		h.HandleKey(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})
}

func TestParsePublicKeyRejectsNonRSAKey(t *testing.T) {
	ecdsaPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ecdsa key: %v", err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&ecdsaPriv.PublicKey)
	if err != nil {
		t.Fatalf("marshal ecdsa public key: %v", err)
	}
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes}))

	_, err = ParsePublicKey(pubPEM)
	if err == nil {
		t.Fatal("expected non-RSA public key to be rejected")
	}
}

func TestHandleSetupRejectsWrongKeyLength(t *testing.T) {
	priv, _ := testRSAKeyPair(t)
	tooShort := []byte("short")
	encrypted, err := EncryptKey(&priv.PublicKey, tooShort)
	if err != nil {
		t.Fatalf("encrypt short key: %v", err)
	}

	body, err := json.Marshal(KeySetupRequest{
		CircuitID:    "short-key",
		EncryptedKey: base64.StdEncoding.EncodeToString(encrypted),
	})
	if err != nil {
		t.Fatalf("marshal setup request: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/setup", bytes.NewReader(body))
	HandleSetup(NewKeyStore(), priv)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

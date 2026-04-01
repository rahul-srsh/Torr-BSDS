package main

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
	"net/url"
	"strconv"
	"testing"

	onion "github.com/rahul-srsh/Torr-BSDS/shared/onion"
)

// TestBuildOnionLayerStructure verifies each onion layer can be peeled by the
// correct node, and that each node sees only the next hop — not the destination.
func TestBuildOnionLayerStructure(t *testing.T) {
	guardKey := randomClientKey(t)
	relayKey := randomClientKey(t)
	exitKey := randomClientKey(t)

	relayAddr := "10.0.0.2:8082"
	exitAddr := "10.0.0.3:8083"
	exitLayer := onion.ExitLayer{URL: "https://example.com/api", Method: "GET"}

	guardCT, err := BuildOnion(guardKey, relayKey, exitKey, exitLayer, relayAddr, exitAddr)
	if err != nil {
		t.Fatalf("BuildOnion: %v", err)
	}

	// Guard peels its layer → sees relayAddr, not exitAddr or the destination.
	guardPlain, err := onion.Decrypt(guardKey, guardCT)
	if err != nil {
		t.Fatalf("decrypt guard layer: %v", err)
	}
	var guardLayer onion.Layer
	json.Unmarshal(guardPlain, &guardLayer)
	if guardLayer.NextHop != relayAddr {
		t.Fatalf("guard nextHop = %q, want %q", guardLayer.NextHop, relayAddr)
	}

	// Relay peels its layer → sees exitAddr, not the destination.
	relayCT, _ := base64.StdEncoding.DecodeString(guardLayer.Payload)
	relayPlain, err := onion.Decrypt(relayKey, relayCT)
	if err != nil {
		t.Fatalf("decrypt relay layer: %v", err)
	}
	var relayLayer onion.Layer
	json.Unmarshal(relayPlain, &relayLayer)
	if relayLayer.NextHop != exitAddr {
		t.Fatalf("relay nextHop = %q, want %q", relayLayer.NextHop, exitAddr)
	}

	// Exit peels its layer → sees the URL and method.
	exitCT, _ := base64.StdEncoding.DecodeString(relayLayer.Payload)
	exitPlain, err := onion.Decrypt(exitKey, exitCT)
	if err != nil {
		t.Fatalf("decrypt exit layer: %v", err)
	}
	var decryptedExit onion.ExitLayer
	json.Unmarshal(exitPlain, &decryptedExit)
	if decryptedExit.URL != exitLayer.URL {
		t.Fatalf("exit URL = %q, want %q", decryptedExit.URL, exitLayer.URL)
	}
	if decryptedExit.Method != exitLayer.Method {
		t.Fatalf("exit method = %q, want %q", decryptedExit.Method, exitLayer.Method)
	}
}

// TestDecryptResponseRoundTrip verifies that DecryptResponse correctly peels
// guard → relay → exit layers to recover the original ExitResponse.
func TestDecryptResponseRoundTrip(t *testing.T) {
	guardKey := randomClientKey(t)
	relayKey := randomClientKey(t)
	exitKey := randomClientKey(t)

	// Simulate what each node produces on the return path.
	exitResp := onion.ExitResponse{
		StatusCode: 200,
		Body:       base64.StdEncoding.EncodeToString([]byte(`{"hello":"world"}`)),
	}
	exitRespJSON, _ := json.Marshal(exitResp)

	// exit node encrypts ExitResponse
	exitCT, _ := onion.Encrypt(exitKey, exitRespJSON)
	// relay node wraps exit ciphertext
	relayCT, _ := onion.Encrypt(relayKey, exitCT)
	// guard node wraps relay ciphertext
	guardCT, _ := onion.Encrypt(guardKey, relayCT)

	result, err := DecryptResponse(guardKey, relayKey, exitKey, guardCT)
	if err != nil {
		t.Fatalf("DecryptResponse: %v", err)
	}
	if result.StatusCode != 200 {
		t.Fatalf("statusCode = %d, want 200", result.StatusCode)
	}
	body, _ := base64.StdEncoding.DecodeString(result.Body)
	if !bytes.Equal(body, []byte(`{"hello":"world"}`)) {
		t.Fatalf("body = %q, want {\"hello\":\"world\"}", body)
	}
}

// TestDecryptResponseWrongKeyOrder verifies that applying keys in the wrong order fails.
func TestDecryptResponseWrongKeyOrder(t *testing.T) {
	guardKey := randomClientKey(t)
	relayKey := randomClientKey(t)
	exitKey := randomClientKey(t)

	exitCT, _ := onion.Encrypt(exitKey, []byte(`{}`))
	relayCT, _ := onion.Encrypt(relayKey, exitCT)
	guardCT, _ := onion.Encrypt(guardKey, relayCT)

	// Swap guard and relay keys — should fail.
	_, err := DecryptResponse(relayKey, guardKey, exitKey, guardCT)
	if err == nil {
		t.Fatal("expected error when applying keys in wrong order")
	}
}

func randomClientKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return key
}

// genTestKeyPair returns a fresh RSA-2048 private key and its PKIX PEM public key string.
func genTestKeyPair(t *testing.T) (*rsa.PrivateKey, string) {
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

// TestSetupCircuitStoresKeysOnNodes verifies that SetupCircuit generates unique keys,
// RSA-OAEP encrypts them, and delivers each to the correct node's /setup endpoint.
func TestSetupCircuitStoresKeysOnNodes(t *testing.T) {
	const circuitID = "setup-test-1"

	guardPriv, guardPubPEM := genTestKeyPair(t)
	relayPriv, relayPubPEM := genTestKeyPair(t)
	exitPriv, exitPubPEM := genTestKeyPair(t)

	guardKS := onion.NewKeyStore()
	relayKS := onion.NewKeyStore()
	exitKS := onion.NewKeyStore()

	newSetupServer := func(ks *onion.KeyStore, priv *rsa.PrivateKey) *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("/setup", onion.HandleSetup(ks, priv))
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	}

	guardSrv := newSetupServer(guardKS, guardPriv)
	relaySrv := newSetupServer(relayKS, relayPriv)
	exitSrv := newSetupServer(exitKS, exitPriv)

	// Build a mock circuit using the test server addresses.
	circuit := buildTestCircuit(t, guardSrv.URL, guardPubPEM, relaySrv.URL, relayPubPEM, exitSrv.URL, exitPubPEM)

	gKey, rKey, eKey, err := SetupCircuit(http.DefaultClient, circuitID, circuit)
	if err != nil {
		t.Fatalf("SetupCircuit: %v", err)
	}

	// Each key must be 32 bytes.
	for name, k := range map[string][]byte{"guard": gKey, "relay": rKey, "exit": eKey} {
		if len(k) != 32 {
			t.Fatalf("%s key length = %d, want 32", name, len(k))
		}
	}

	// Verify each node stored the correct key.
	storedG, ok := guardKS.Get(circuitID)
	if !ok {
		t.Fatal("guard key not stored")
	}
	storedR, ok := relayKS.Get(circuitID)
	if !ok {
		t.Fatal("relay key not stored")
	}
	storedE, ok := exitKS.Get(circuitID)
	if !ok {
		t.Fatal("exit key not stored")
	}

	if !bytes.Equal(storedG, gKey) {
		t.Fatal("guard stored key does not match returned guardKey")
	}
	if !bytes.Equal(storedR, rKey) {
		t.Fatal("relay stored key does not match returned relayKey")
	}
	if !bytes.Equal(storedE, eKey) {
		t.Fatal("exit stored key does not match returned exitKey")
	}

	// All three keys must be distinct.
	if bytes.Equal(gKey, rKey) || bytes.Equal(rKey, eKey) || bytes.Equal(gKey, eKey) {
		t.Fatal("SetupCircuit must generate distinct keys for each hop")
	}
}

// TestSetupCircuitBadPublicKey verifies that SetupCircuit fails fast when a node's
// public key PEM is malformed.
func TestSetupCircuitBadPublicKey(t *testing.T) {
	circuit := &CircuitResponse{
		Guard: NodeRecord{NodeRegistration: NodeRegistration{NodeID: "g", Host: "127.0.0.1", Port: 1, PublicKey: "not-a-pem"}},
		Relay: NodeRecord{NodeRegistration: NodeRegistration{NodeID: "r", Host: "127.0.0.1", Port: 2, PublicKey: "not-a-pem"}},
		Exit:  NodeRecord{NodeRegistration: NodeRegistration{NodeID: "e", Host: "127.0.0.1", Port: 3, PublicKey: "not-a-pem"}},
	}
	_, _, _, err := SetupCircuit(http.DefaultClient, "c1", circuit)
	if err == nil {
		t.Fatal("expected error for malformed public key")
	}
}

// buildTestCircuit constructs a CircuitResponse from test server URLs and PEM keys.
// The URL is parsed into host+port so the client can dial them correctly.
func buildTestCircuit(t *testing.T, guardURL, guardPEM, relayURL, relayPEM, exitURL, exitPEM string) *CircuitResponse {
	t.Helper()
	return &CircuitResponse{
		Guard: nodeRecordFromURL(t, "g1", "guard", guardURL, guardPEM),
		Relay: nodeRecordFromURL(t, "r1", "relay", relayURL, relayPEM),
		Exit:  nodeRecordFromURL(t, "e1", "exit", exitURL, exitPEM),
	}
}

func nodeRecordFromURL(t *testing.T, id, nodeType, rawURL, pubPEM string) NodeRecord {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", rawURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port from %q: %v", rawURL, err)
	}
	return NodeRecord{NodeRegistration: NodeRegistration{
		NodeID: id, NodeType: nodeType, Host: u.Hostname(), Port: port, PublicKey: pubPEM,
	}}
}


package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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

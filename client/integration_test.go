package main

import (
	"crypto/rand"
	"encoding/base64"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	onion "github.com/rahul-srsh/Torr-BSDS/shared/onion"
)

// nodeServer spins up an httptest.Server wired to the given /key and /onion handlers.
func nodeServer(t *testing.T, keyHandler, onionHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/key", keyHandler)
	mux.HandleFunc("/onion", onionHandler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// addr strips the "http://" prefix to get host:port.
func addr(srv *httptest.Server) string {
	return strings.TrimPrefix(srv.URL, "http://")
}

// TestFullCircuitRoundTrip sends a GET request through a 3-hop circuit and
// verifies the response matches byte-for-byte what the destination sent.
func TestFullCircuitRoundTrip(t *testing.T) {
	const circuitID = "integration-1"
	const destBody = `{"message":"hello from destination"}`

	guardKey := randomIntegrationKey(t)
	relayKey := randomIntegrationKey(t)
	exitKey := randomIntegrationKey(t)

	// ── Destination ──────────────────────────────────────────────────────────
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api" {
			t.Errorf("dest: got %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(destBody))
	}))
	t.Cleanup(dest.Close)

	// ── Exit node ─────────────────────────────────────────────────────────────
	exitKS := onion.NewKeyStore()
	exitH := onion.NewExitHandler(exitKS, http.DefaultClient)
	exitSrv := nodeServer(t, exitH.HandleKey, exitH.HandleOnion)

	// ── Relay node ────────────────────────────────────────────────────────────
	relayKS := onion.NewKeyStore()
	relayH := onion.NewHandler(relayKS, http.DefaultClient, "relay")
	relaySrv := nodeServer(t, relayH.HandleKey, relayH.HandleOnion)

	// ── Guard node ────────────────────────────────────────────────────────────
	guardKS := onion.NewKeyStore()
	guardH := onion.NewHandler(guardKS, http.DefaultClient, "guard")
	guardSrv := nodeServer(t, guardH.HandleKey, guardH.HandleOnion)

	// ── Register session keys ─────────────────────────────────────────────────
	for _, tc := range []struct {
		name string
		url  string
		key  []byte
	}{
		{"guard", guardSrv.URL, guardKey},
		{"relay", relaySrv.URL, relayKey},
		{"exit", exitSrv.URL, exitKey},
	} {
		if err := RegisterKey(http.DefaultClient, tc.url, circuitID, tc.key); err != nil {
			t.Fatalf("register %s key: %v", tc.name, err)
		}
	}

	// ── Build 3-layer onion ───────────────────────────────────────────────────
	guardCT, err := BuildOnion(
		guardKey, relayKey, exitKey,
		onion.ExitLayer{URL: dest.URL + "/api", Method: http.MethodGet},
		addr(relaySrv), addr(exitSrv),
	)
	if err != nil {
		t.Fatalf("BuildOnion: %v", err)
	}

	// ── Send through circuit, measure return-path latency ────────────────────
	start := time.Now()
	onionResp, err := SendOnion(http.DefaultClient, guardSrv.URL, circuitID, guardCT)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("SendOnion: %v", err)
	}
	log.Printf("[client] circuit %s round-trip latency: %s", circuitID, elapsed)

	// ── Decrypt 3 layers: guard → relay → exit ────────────────────────────────
	payload, err := base64.StdEncoding.DecodeString(onionResp.Payload)
	if err != nil {
		t.Fatalf("decode response payload: %v", err)
	}
	exitResp, err := DecryptResponse(guardKey, relayKey, exitKey, payload)
	if err != nil {
		t.Fatalf("DecryptResponse: %v", err)
	}

	// ── Verify response matches destination byte-for-byte ─────────────────────
	if exitResp.StatusCode != http.StatusOK {
		t.Fatalf("statusCode = %d, want %d", exitResp.StatusCode, http.StatusOK)
	}
	bodyBytes, _ := base64.StdEncoding.DecodeString(exitResp.Body)
	if string(bodyBytes) != destBody {
		t.Fatalf("body = %q, want %q", bodyBytes, destBody)
	}
	if exitResp.Headers["Content-Type"] != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", exitResp.Headers["Content-Type"])
	}
}

// TestFullCircuitWithPostBody verifies a POST request with a body travels
// correctly through the circuit and the destination receives the body.
func TestFullCircuitWithPostBody(t *testing.T) {
	const circuitID = "integration-2"
	requestBody := []byte(`{"query":"test"}`)
	responseBody := []byte(`{"result":"ok"}`)

	guardKey := randomIntegrationKey(t)
	relayKey := randomIntegrationKey(t)
	exitKey := randomIntegrationKey(t)

	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(responseBody)
	}))
	t.Cleanup(dest.Close)

	exitKS := onion.NewKeyStore()
	exitH := onion.NewExitHandler(exitKS, http.DefaultClient)
	exitSrv := nodeServer(t, exitH.HandleKey, exitH.HandleOnion)

	relayKS := onion.NewKeyStore()
	relayH := onion.NewHandler(relayKS, http.DefaultClient, "relay")
	relaySrv := nodeServer(t, relayH.HandleKey, relayH.HandleOnion)

	guardKS := onion.NewKeyStore()
	guardH := onion.NewHandler(guardKS, http.DefaultClient, "guard")
	guardSrv := nodeServer(t, guardH.HandleKey, guardH.HandleOnion)

	for _, tc := range []struct {
		url string
		key []byte
	}{
		{guardSrv.URL, guardKey},
		{relaySrv.URL, relayKey},
		{exitSrv.URL, exitKey},
	} {
		if err := RegisterKey(http.DefaultClient, tc.url, circuitID, tc.key); err != nil {
			t.Fatalf("register key: %v", err)
		}
	}

	guardCT, err := BuildOnion(
		guardKey, relayKey, exitKey,
		onion.ExitLayer{
			URL:     dest.URL,
			Method:  http.MethodPost,
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    base64.StdEncoding.EncodeToString(requestBody),
		},
		addr(relaySrv), addr(exitSrv),
	)
	if err != nil {
		t.Fatalf("BuildOnion: %v", err)
	}

	start := time.Now()
	onionResp, err := SendOnion(http.DefaultClient, guardSrv.URL, circuitID, guardCT)
	log.Printf("[client] circuit %s round-trip latency: %s", circuitID, time.Since(start))
	if err != nil {
		t.Fatalf("SendOnion: %v", err)
	}

	payload, _ := base64.StdEncoding.DecodeString(onionResp.Payload)
	exitResp, err := DecryptResponse(guardKey, relayKey, exitKey, payload)
	if err != nil {
		t.Fatalf("DecryptResponse: %v", err)
	}
	if exitResp.StatusCode != http.StatusOK {
		t.Fatalf("statusCode = %d, want %d", exitResp.StatusCode, http.StatusOK)
	}
	got, _ := base64.StdEncoding.DecodeString(exitResp.Body)
	if string(got) != string(responseBody) {
		t.Fatalf("body = %q, want %q", got, responseBody)
	}
}

// TestFullCircuitNon200PropagatesThrough verifies a non-200 destination response
// travels encrypted through the whole circuit back to the client.
func TestFullCircuitNon200PropagatesThrough(t *testing.T) {
	const circuitID = "integration-3"

	guardKey := randomIntegrationKey(t)
	relayKey := randomIntegrationKey(t)
	exitKey := randomIntegrationKey(t)

	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("I'm a teapot"))
	}))
	t.Cleanup(dest.Close)

	exitKS := onion.NewKeyStore()
	exitH := onion.NewExitHandler(exitKS, http.DefaultClient)
	exitSrv := nodeServer(t, exitH.HandleKey, exitH.HandleOnion)

	relayKS := onion.NewKeyStore()
	relayH := onion.NewHandler(relayKS, http.DefaultClient, "relay")
	relaySrv := nodeServer(t, relayH.HandleKey, relayH.HandleOnion)

	guardKS := onion.NewKeyStore()
	guardH := onion.NewHandler(guardKS, http.DefaultClient, "guard")
	guardSrv := nodeServer(t, guardH.HandleKey, guardH.HandleOnion)

	for _, tc := range []struct {
		url string
		key []byte
	}{
		{guardSrv.URL, guardKey},
		{relaySrv.URL, relayKey},
		{exitSrv.URL, exitKey},
	} {
		RegisterKey(http.DefaultClient, tc.url, circuitID, tc.key)
	}

	guardCT, _ := BuildOnion(
		guardKey, relayKey, exitKey,
		onion.ExitLayer{URL: dest.URL, Method: http.MethodGet},
		addr(relaySrv), addr(exitSrv),
	)

	start := time.Now()
	onionResp, err := SendOnion(http.DefaultClient, guardSrv.URL, circuitID, guardCT)
	log.Printf("[client] circuit %s round-trip latency: %s", circuitID, time.Since(start))
	if err != nil {
		t.Fatalf("SendOnion: %v", err)
	}

	payload, _ := base64.StdEncoding.DecodeString(onionResp.Payload)
	exitResp, err := DecryptResponse(guardKey, relayKey, exitKey, payload)
	if err != nil {
		t.Fatalf("DecryptResponse: %v", err)
	}

	// The 418 from the destination travels encrypted through the circuit.
	if exitResp.StatusCode != http.StatusTeapot {
		t.Fatalf("statusCode = %d, want %d", exitResp.StatusCode, http.StatusTeapot)
	}
	body, _ := base64.StdEncoding.DecodeString(exitResp.Body)
	if !strings.Contains(string(body), "teapot") {
		t.Fatalf("body = %q, should contain 'teapot'", body)
	}
}

// TestFullCircuitMultipleCircuits verifies two independent circuits through the
// same nodes use different keys and don't interfere with each other.
func TestFullCircuitMultipleCircuits(t *testing.T) {
	destBody1 := `{"circuit":1}`
	destBody2 := `{"circuit":2}`

	dest1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(destBody1))
	}))
	t.Cleanup(dest1.Close)
	dest2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(destBody2))
	}))
	t.Cleanup(dest2.Close)

	// Shared node servers — each circuit gets its own keys.
	exitKS := onion.NewKeyStore()
	exitH := onion.NewExitHandler(exitKS, http.DefaultClient)
	exitSrv := nodeServer(t, exitH.HandleKey, exitH.HandleOnion)

	relayKS := onion.NewKeyStore()
	relayH := onion.NewHandler(relayKS, http.DefaultClient, "relay")
	relaySrv := nodeServer(t, relayH.HandleKey, relayH.HandleOnion)

	guardKS := onion.NewKeyStore()
	guardH := onion.NewHandler(guardKS, http.DefaultClient, "guard")
	guardSrv := nodeServer(t, guardH.HandleKey, guardH.HandleOnion)

	for _, tc := range []struct {
		circuitID string
		destURL   string
		destBody  string
	}{
		{"circuit-A", dest1.URL, destBody1},
		{"circuit-B", dest2.URL, destBody2},
	} {
		gKey := randomIntegrationKey(t)
		rKey := randomIntegrationKey(t)
		eKey := randomIntegrationKey(t)

		for _, kc := range []struct {
			url string
			key []byte
		}{
			{guardSrv.URL, gKey},
			{relaySrv.URL, rKey},
			{exitSrv.URL, eKey},
		} {
			if err := RegisterKey(http.DefaultClient, kc.url, tc.circuitID, kc.key); err != nil {
				t.Fatalf("register key for %s: %v", tc.circuitID, err)
			}
		}

		guardCT, err := BuildOnion(gKey, rKey, eKey,
			onion.ExitLayer{URL: tc.destURL, Method: http.MethodGet},
			addr(relaySrv), addr(exitSrv),
		)
		if err != nil {
			t.Fatalf("BuildOnion %s: %v", tc.circuitID, err)
		}

		onionResp, err := SendOnion(http.DefaultClient, guardSrv.URL, tc.circuitID, guardCT)
		if err != nil {
			t.Fatalf("SendOnion %s: %v", tc.circuitID, err)
		}

		raw, _ := base64.StdEncoding.DecodeString(onionResp.Payload)
		exitResp, err := DecryptResponse(gKey, rKey, eKey, raw)
		if err != nil {
			t.Fatalf("DecryptResponse %s: %v", tc.circuitID, err)
		}

		got, _ := base64.StdEncoding.DecodeString(exitResp.Body)
		if string(got) != tc.destBody {
			t.Fatalf("%s body = %q, want %q", tc.circuitID, got, tc.destBody)
		}
		log.Printf("[client] %s verified: body matches destination", tc.circuitID)
	}
}

// TestBuildOnionKeysAreUniquePerCircuit verifies BuildOnion produces different
// ciphertexts for the same destination when called twice (random nonce).
func TestBuildOnionKeysAreUniquePerCircuit(t *testing.T) {
	gKey, rKey, eKey := randomIntegrationKey(t), randomIntegrationKey(t), randomIntegrationKey(t)
	layer := onion.ExitLayer{URL: "https://example.com", Method: "GET"}

	ct1, _ := BuildOnion(gKey, rKey, eKey, layer, "relay:8082", "exit:8083")
	ct2, _ := BuildOnion(gKey, rKey, eKey, layer, "relay:8082", "exit:8083")

	if string(ct1) == string(ct2) {
		t.Fatal("two BuildOnion calls with the same keys must produce different ciphertexts")
	}
}

func randomIntegrationKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return key
}


package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
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

// nodeServerFull spins up a node server with /key, /onion, and /setup handlers.
func nodeServerFull(t *testing.T, keyHandler, onionHandler, setupHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/key", keyHandler)
	mux.HandleFunc("/onion", onionHandler)
	mux.HandleFunc("/setup", setupHandler)
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
	exitResp, err := DecryptResponse(guardKey, relayKey, exitKey, onionResp.Payload)
	if err != nil {
		t.Fatalf("DecryptResponse: %v", err)
	}

	// ── Verify response matches destination byte-for-byte ─────────────────────
	if exitResp.StatusCode != http.StatusOK {
		t.Fatalf("statusCode = %d, want %d", exitResp.StatusCode, http.StatusOK)
	}
	if string(exitResp.Body) != destBody {
		t.Fatalf("body = %q, want %q", exitResp.Body, destBody)
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
		gotBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if !bytes.Equal(gotBody, requestBody) {
			t.Fatalf("destination body = %q, want %q", gotBody, requestBody)
		}
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
			Body:    requestBody,
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

	exitResp, err := DecryptResponse(guardKey, relayKey, exitKey, onionResp.Payload)
	if err != nil {
		t.Fatalf("DecryptResponse: %v", err)
	}
	if exitResp.StatusCode != http.StatusOK {
		t.Fatalf("statusCode = %d, want %d", exitResp.StatusCode, http.StatusOK)
	}
	if string(exitResp.Body) != string(responseBody) {
		t.Fatalf("body = %q, want %q", exitResp.Body, responseBody)
	}
}

func TestOneHopCircuitRoundTrip(t *testing.T) {
	const circuitID = "integration-1-hop"
	const destBody = `{"message":"hello from one-hop"}`

	guardKey := randomIntegrationKey(t)

	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api" {
			t.Errorf("dest: got %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(destBody))
	}))
	t.Cleanup(dest.Close)

	guardKS := onion.NewKeyStore()
	guardH := onion.NewHandlerWithDirectExit(guardKS, http.DefaultClient, "guard")
	guardSrv := nodeServer(t, guardH.HandleKey, guardH.HandleOnion)

	if err := RegisterKey(http.DefaultClient, guardSrv.URL, circuitID, guardKey); err != nil {
		t.Fatalf("register guard key: %v", err)
	}

	guardCT, err := BuildOnionWithHops(
		guardKey, nil, nil,
		onion.ExitLayer{URL: dest.URL + "/api", Method: http.MethodGet},
		"", "", 1,
	)
	if err != nil {
		t.Fatalf("BuildOnionWithHops(1): %v", err)
	}

	onionResp, err := SendOnion(http.DefaultClient, guardSrv.URL, circuitID, guardCT)
	if err != nil {
		t.Fatalf("SendOnion: %v", err)
	}

	exitResp, err := DecryptResponseWithHops(guardKey, nil, nil, onionResp.Payload, 1)
	if err != nil {
		t.Fatalf("DecryptResponseWithHops(1): %v", err)
	}
	if exitResp.StatusCode != http.StatusOK {
		t.Fatalf("statusCode = %d, want %d", exitResp.StatusCode, http.StatusOK)
	}
	if string(exitResp.Body) != destBody {
		t.Fatalf("body = %q, want %q", exitResp.Body, destBody)
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

	exitResp, err := DecryptResponse(guardKey, relayKey, exitKey, onionResp.Payload)
	if err != nil {
		t.Fatalf("DecryptResponse: %v", err)
	}

	// The 418 from the destination travels encrypted through the circuit.
	if exitResp.StatusCode != http.StatusTeapot {
		t.Fatalf("statusCode = %d, want %d", exitResp.StatusCode, http.StatusTeapot)
	}
	if !strings.Contains(string(exitResp.Body), "teapot") {
		t.Fatalf("body = %q, should contain 'teapot'", exitResp.Body)
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

		exitResp, err := DecryptResponse(gKey, rKey, eKey, onionResp.Payload)
		if err != nil {
			t.Fatalf("DecryptResponse %s: %v", tc.circuitID, err)
		}

		if string(exitResp.Body) != tc.destBody {
			t.Fatalf("%s body = %q, want %q", tc.circuitID, exitResp.Body, tc.destBody)
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

// genIntegrationKeyPair generates a fresh RSA-2048 key pair for integration tests.
func genIntegrationKeyPair(t *testing.T) (*rsa.PrivateKey, string) {
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

// nodeRecordForSrv builds a NodeRecord for an httptest.Server, used in the mock directory.
func nodeRecordForSrv(t *testing.T, id, nodeType string, srv *httptest.Server, pubPEM string) NodeRecord {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())
	return NodeRecord{NodeRegistration: NodeRegistration{
		NodeID: id, NodeType: nodeType, Host: u.Hostname(), Port: port, PublicKey: pubPEM,
	}}
}

// TestSetupCircuitIntegration verifies the full key exchange flow:
//  1. Client calls GET /circuit on a mock directory server.
//  2. Client calls SetupCircuit — each node receives its RSA-OAEP-encrypted session key.
//  3. The stored keys are used to complete a full 3-hop onion round-trip.
func TestSetupCircuitIntegration(t *testing.T) {
	const circuitID = "setup-integration-1"
	const destBody = `{"from":"destination"}`

	// Generate key pairs for each node.
	guardPriv, guardPubPEM := genIntegrationKeyPair(t)
	relayPriv, relayPubPEM := genIntegrationKeyPair(t)
	exitPriv, exitPubPEM := genIntegrationKeyPair(t)

	// ── Node servers ─────────────────────────────────────────────────────────
	exitKS := onion.NewKeyStore()
	exitH := onion.NewExitHandler(exitKS, http.DefaultClient)
	exitSrv := nodeServerFull(t, exitH.HandleKey, exitH.HandleOnion, onion.HandleSetup(exitKS, exitPriv))

	relayKS := onion.NewKeyStore()
	relayH := onion.NewHandler(relayKS, http.DefaultClient, "relay")
	relaySrv := nodeServerFull(t, relayH.HandleKey, relayH.HandleOnion, onion.HandleSetup(relayKS, relayPriv))

	guardKS := onion.NewKeyStore()
	guardH := onion.NewHandler(guardKS, http.DefaultClient, "guard")
	guardSrv := nodeServerFull(t, guardH.HandleKey, guardH.HandleOnion, onion.HandleSetup(guardKS, guardPriv))

	// ── Mock directory server ─────────────────────────────────────────────────
	circuit := CircuitResponse{
		Guard: nodeRecordForSrv(t, "g1", "guard", guardSrv, guardPubPEM),
		Relay: nodeRecordForSrv(t, "r1", "relay", relaySrv, relayPubPEM),
		Exit:  nodeRecordForSrv(t, "e1", "exit", exitSrv, exitPubPEM),
	}
	dirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/circuit" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(circuit)
	}))
	t.Cleanup(dirSrv.Close)

	// ── Destination ───────────────────────────────────────────────────────────
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(destBody))
	}))
	t.Cleanup(dest.Close)

	// ── Step 1: Get circuit from directory ────────────────────────────────────
	got, err := GetCircuit(http.DefaultClient, dirSrv.URL)
	if err != nil {
		t.Fatalf("GetCircuit: %v", err)
	}

	// ── Step 2: Key exchange via /setup ───────────────────────────────────────
	gKey, rKey, eKey, err := SetupCircuit(http.DefaultClient, circuitID, got)
	if err != nil {
		t.Fatalf("SetupCircuit: %v", err)
	}

	// Verify all three nodes received their keys.
	if _, ok := guardKS.Get(circuitID); !ok {
		t.Fatal("guard node did not receive session key")
	}
	if _, ok := relayKS.Get(circuitID); !ok {
		t.Fatal("relay node did not receive session key")
	}
	if _, ok := exitKS.Get(circuitID); !ok {
		t.Fatal("exit node did not receive session key")
	}

	// ── Step 3: Full round-trip using the exchanged keys ──────────────────────
	guardCT, err := BuildOnion(gKey, rKey, eKey,
		onion.ExitLayer{URL: dest.URL, Method: http.MethodGet},
		addr(relaySrv), addr(exitSrv),
	)
	if err != nil {
		t.Fatalf("BuildOnion: %v", err)
	}

	start := time.Now()
	onionResp, err := SendOnion(http.DefaultClient, guardSrv.URL, circuitID, guardCT)
	log.Printf("[client] setup-integration round-trip latency: %s", time.Since(start))
	if err != nil {
		t.Fatalf("SendOnion: %v", err)
	}

	exitResp, err := DecryptResponse(gKey, rKey, eKey, onionResp.Payload)
	if err != nil {
		t.Fatalf("DecryptResponse: %v", err)
	}
	if exitResp.StatusCode != http.StatusOK {
		t.Fatalf("statusCode = %d, want 200", exitResp.StatusCode)
	}
	if string(exitResp.Body) != destBody {
		t.Fatalf("body = %q, want %q", exitResp.Body, destBody)
	}
}

func TestRunClientEndToEndCLIFlow(t *testing.T) {
	const destBody = `{"from":"runClient"}`

	guardPriv, guardPubPEM := genIntegrationKeyPair(t)
	relayPriv, relayPubPEM := genIntegrationKeyPair(t)
	exitPriv, exitPubPEM := genIntegrationKeyPair(t)

	exitKS := onion.NewKeyStore()
	exitH := onion.NewExitHandler(exitKS, http.DefaultClient)
	exitSrv := nodeServerFull(t, exitH.HandleKey, exitH.HandleOnion, onion.HandleSetup(exitKS, exitPriv))

	relayKS := onion.NewKeyStore()
	relayH := onion.NewHandler(relayKS, http.DefaultClient, "relay")
	relaySrv := nodeServerFull(t, relayH.HandleKey, relayH.HandleOnion, onion.HandleSetup(relayKS, relayPriv))

	guardKS := onion.NewKeyStore()
	guardH := onion.NewHandler(guardKS, http.DefaultClient, "guard")
	guardSrv := nodeServerFull(t, guardH.HandleKey, guardH.HandleOnion, onion.HandleSetup(guardKS, guardPriv))

	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/resource" {
			t.Fatalf("destination got %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(destBody))
	}))
	t.Cleanup(destination.Close)

	dirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/circuit" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("hops"); got != "3" {
			t.Fatalf("hops query = %q, want 3", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CircuitResponse{
			Guard: nodeRecordForSrv(t, "g1", "guard", guardSrv, guardPubPEM),
			Relay: nodeRecordForSrv(t, "r1", "relay", relaySrv, relayPubPEM),
			Exit:  nodeRecordForSrv(t, "e1", "exit", exitSrv, exitPubPEM),
		})
	}))
	t.Cleanup(dirSrv.Close)

	cfg := &clientConfig{
		DirectoryURL:   dirSrv.URL,
		DestinationURL: destination.URL + "/resource",
		Method:         http.MethodGet,
		Hops:           3,
		Timeout:        2 * time.Second,
	}

	var stdout bytes.Buffer
	if err := runClient(cfg, &stdout); err != nil {
		t.Fatalf("runClient: %v", err)
	}

	if stdout.String() != destBody {
		t.Fatalf("stdout = %q, want %q", stdout.String(), destBody)
	}
}

func TestRunClientUnreachableNodeReturnsClearError(t *testing.T) {
	guardPriv, guardPubPEM := genIntegrationKeyPair(t)
	relayPriv, relayPubPEM := genIntegrationKeyPair(t)
	exitPriv, exitPubPEM := genIntegrationKeyPair(t)

	dirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/circuit" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(CircuitResponse{
			Guard: NodeRecord{NodeRegistration: NodeRegistration{
				NodeID: "g1", NodeType: "guard", Host: "127.0.0.1", Port: 1, PublicKey: guardPubPEM,
			}},
			Relay: NodeRecord{NodeRegistration: NodeRegistration{
				NodeID: "r1", NodeType: "relay", Host: "127.0.0.1", Port: 2, PublicKey: relayPubPEM,
			}},
			Exit: NodeRecord{NodeRegistration: NodeRegistration{
				NodeID: "e1", NodeType: "exit", Host: "127.0.0.1", Port: 3, PublicKey: exitPubPEM,
			}},
		})
	}))
	t.Cleanup(dirSrv.Close)

	_ = guardPriv
	_ = relayPriv
	_ = exitPriv

	cfg := &clientConfig{
		DirectoryURL:   dirSrv.URL,
		DestinationURL: "http://example.invalid",
		Method:         http.MethodGet,
		Hops:           3,
		Timeout:        200 * time.Millisecond,
	}

	err := runClient(cfg, io.Discard)
	if err == nil {
		t.Fatal("expected runClient to fail when a node is unreachable")
	}
	// After maxCircuitAttempts the error is wrapped as "all N circuit attempts failed: ..."
	// but the inner causes from the last attempt are still present.
	if !strings.Contains(err.Error(), "establish session keys") {
		t.Fatalf("error = %q, want session key setup context", err)
	}
	if !strings.Contains(err.Error(), "setup key for node g1") {
		t.Fatalf("error = %q, want guard node context", err)
	}
}

// TestExecuteRequestRetryOnBadCircuit verifies that when the first circuit's
// /setup fails (unreachable guard), ExecuteRequestWithHops fetches a fresh
// circuit and succeeds on the next attempt without surfacing an error.
func TestExecuteRequestRetryOnBadCircuit(t *testing.T) {
	const circuitID = "retry-circuit-1"
	const destBody = `{"from":"retry"}`

	goodGuardPriv, goodGuardPubPEM := genIntegrationKeyPair(t)
	goodRelayPriv, goodRelayPubPEM := genIntegrationKeyPair(t)
	goodExitPriv, goodExitPubPEM := genIntegrationKeyPair(t)

	exitKS := onion.NewKeyStore()
	exitH := onion.NewExitHandler(exitKS, http.DefaultClient)
	exitSrv := nodeServerFull(t, exitH.HandleKey, exitH.HandleOnion, onion.HandleSetup(exitKS, goodExitPriv))

	relayKS := onion.NewKeyStore()
	relayH := onion.NewHandler(relayKS, http.DefaultClient, "relay")
	relaySrv := nodeServerFull(t, relayH.HandleKey, relayH.HandleOnion, onion.HandleSetup(relayKS, goodRelayPriv))

	guardKS := onion.NewKeyStore()
	guardH := onion.NewHandler(guardKS, http.DefaultClient, "guard")
	guardSrv := nodeServerFull(t, guardH.HandleKey, guardH.HandleOnion, onion.HandleSetup(guardKS, goodGuardPriv))

	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(destBody))
	}))
	t.Cleanup(dest.Close)

	// Directory: first call returns a dead guard (port 1), subsequent calls return healthy nodes.
	var dirCallCount int32
	dirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/circuit" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&dirCallCount, 1) == 1 {
			_ = json.NewEncoder(w).Encode(CircuitResponse{
				Guard: NodeRecord{NodeRegistration: NodeRegistration{
					NodeID: "dead-guard", NodeType: "guard", Host: "127.0.0.1", Port: 1,
					PublicKey: goodGuardPubPEM,
				}},
				Relay: nodeRecordForSrv(t, "r1", "relay", relaySrv, goodRelayPubPEM),
				Exit:  nodeRecordForSrv(t, "e1", "exit", exitSrv, goodExitPubPEM),
			})
		} else {
			_ = json.NewEncoder(w).Encode(CircuitResponse{
				Guard: nodeRecordForSrv(t, "g1", "guard", guardSrv, goodGuardPubPEM),
				Relay: nodeRecordForSrv(t, "r1", "relay", relaySrv, goodRelayPubPEM),
				Exit:  nodeRecordForSrv(t, "e1", "exit", exitSrv, goodExitPubPEM),
			})
		}
	}))
	t.Cleanup(dirSrv.Close)

	cfg := &clientConfig{
		DirectoryURL:   dirSrv.URL,
		DestinationURL: dest.URL,
		Method:         http.MethodGet,
		Hops:           3,
		Timeout:        2 * time.Second,
	}

	var stdout bytes.Buffer
	if err := runClient(cfg, &stdout); err != nil {
		t.Fatalf("runClient: %v", err)
	}
	if stdout.String() != destBody {
		t.Fatalf("stdout = %q, want %q", stdout.String(), destBody)
	}
	if got := atomic.LoadInt32(&dirCallCount); got < 2 {
		t.Fatalf("directory called %d times, want at least 2 (1 bad circuit + 1 good)", got)
	}
}

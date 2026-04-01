package main

// Live end-to-end test against the real AWS cluster.
// Skipped unless DIRECTORY_SERVER_URL and GUARD_PUBLIC_IP are set.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	onion "github.com/rahul-srsh/Torr-BSDS/shared/onion"
)

func TestLiveCircuitEndToEnd(t *testing.T) {
	directoryURL := os.Getenv("DIRECTORY_SERVER_URL")
	guardPublicIP := os.Getenv("GUARD_PUBLIC_IP")
	echoURL := os.Getenv("ECHO_SERVER_URL")
	if directoryURL == "" || guardPublicIP == "" || echoURL == "" {
		t.Skip("DIRECTORY_SERVER_URL, GUARD_PUBLIC_IP, ECHO_SERVER_URL not set — skipping live test")
	}

	client := &http.Client{Timeout: 15 * time.Second}
	circuitID := fmt.Sprintf("live-%d", time.Now().UnixMilli())

	// ── Step 1: Get circuit ───────────────────────────────────────────────────
	t.Log("fetching circuit from directory server...")
	circuit, err := GetCircuit(client, directoryURL)
	if err != nil {
		t.Fatalf("GetCircuit: %v", err)
	}
	t.Logf("circuit: guard=%s relay=%s exit=%s",
		circuit.Guard.Host, circuit.Relay.Host, circuit.Exit.Host)

	// ── Step 2: RSA key exchange ──────────────────────────────────────────────
	// Override guard host with its public IP so our local machine can reach it.
	circuit.Guard.Host = guardPublicIP

	t.Log("running SetupCircuit (RSA-OAEP key exchange with all 3 nodes)...")
	guardKey, relayKey, exitKey, err := SetupCircuit(client, circuitID, circuit)
	if err != nil {
		t.Fatalf("SetupCircuit: %v", err)
	}
	t.Log("session keys established on guard, relay, exit")

	// ── Step 3: Build 3-layer onion ───────────────────────────────────────────
	exitLayer := onion.ExitLayer{
		URL:    echoURL + "/echo",
		Method: http.MethodPost,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:   base64.StdEncoding.EncodeToString([]byte(`{"message":"hello from onion client"}`)),
	}
	relayAddr := fmt.Sprintf("%s:%d", circuit.Relay.Host, circuit.Relay.Port)
	exitAddr := fmt.Sprintf("%s:%d", circuit.Exit.Host, circuit.Exit.Port)

	guardCT, err := BuildOnion(guardKey, relayKey, exitKey, exitLayer, relayAddr, exitAddr)
	if err != nil {
		t.Fatalf("BuildOnion: %v", err)
	}

	// ── Step 4: Send onion through guard ─────────────────────────────────────
	guardURL := fmt.Sprintf("http://%s:%d", guardPublicIP, circuit.Guard.Port)
	t.Logf("sending onion to guard %s...", guardURL)
	start := time.Now()
	onionResp, err := SendOnion(client, guardURL, circuitID, guardCT)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("SendOnion: %v", err)
	}
	t.Logf("response received in %s", elapsed)

	// ── Step 5: Decrypt 3-layer response ─────────────────────────────────────
	raw, err := base64.StdEncoding.DecodeString(onionResp.Payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	exitResp, err := DecryptResponse(guardKey, relayKey, exitKey, raw)
	if err != nil {
		t.Fatalf("DecryptResponse: %v", err)
	}

	if exitResp.StatusCode != http.StatusOK {
		t.Fatalf("exit status = %d, want 200", exitResp.StatusCode)
	}

	body, _ := base64.StdEncoding.DecodeString(exitResp.Body)
	var pretty map[string]any
	if json.Unmarshal(body, &pretty) == nil {
		out, _ := json.MarshalIndent(pretty, "", "  ")
		t.Logf("echo response:\n%s", out)
	}

	t.Log("END-TO-END ONION ROUTING PASSED")
}

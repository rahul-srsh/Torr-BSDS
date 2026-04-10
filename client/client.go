package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	onion "github.com/rahul-srsh/Torr-BSDS/shared/onion"
)

// maxCircuitAttempts is the number of times ExecuteRequestWithHops will fetch
// a fresh circuit and retry before giving up. A setup or send failure means a
// node in the selected circuit is unhealthy; a new circuit may avoid it.
const maxCircuitAttempts = 3

// NodeRegistration mirrors the directory server's registration payload.
type NodeRegistration struct {
	NodeID    string `json:"nodeId"`
	NodeType  string `json:"nodeType"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	PublicKey string `json:"publicKey"`
}

// NodeRecord mirrors the directory server's node record (NodeRegistration + metadata).
type NodeRecord struct {
	NodeRegistration
	LastSeen time.Time `json:"lastSeen"`
	Status   string    `json:"status"`
}

// CircuitResponse mirrors the directory server's GET /circuit response.
type CircuitResponse struct {
	Guard NodeRecord `json:"guard"`
	Relay NodeRecord `json:"relay"`
	Exit  NodeRecord `json:"exit"`
}

// ExecuteRequest runs the full 3-hop client flow against the onion network.
func ExecuteRequest(client *http.Client, directoryURL, circuitID string, exitLayer onion.ExitLayer) (*onion.ExitResponse, error) {
	return ExecuteRequestWithHops(client, directoryURL, circuitID, exitLayer, 3)
}

// ExecuteRequestWithHops runs the full client flow for the requested hop count:
// circuit lookup, key exchange, onion wrapping, send via guard, and response unwrap.
//
// A setup or send failure indicates a node in the selected circuit is unhealthy.
// ExecuteRequestWithHops fetches a fresh circuit and retries up to maxCircuitAttempts
// times before returning an error. Directory, build, and decrypt failures are not
// retried: they are not circuit-quality issues and a new circuit will not help.
func ExecuteRequestWithHops(client *http.Client, directoryURL, circuitID string, exitLayer onion.ExitLayer, hops int) (*onion.ExitResponse, error) {
	if err := validateHops(hops); err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 1; attempt <= maxCircuitAttempts; attempt++ {
		if attempt > 1 {
			log.Printf("[client] circuit attempt %d/%d failed (%v); fetching fresh circuit", attempt-1, maxCircuitAttempts, lastErr)
		}

		circuit, err := GetCircuitWithHops(client, directoryURL, hops)
		if err != nil {
			return nil, fmt.Errorf("get circuit from directory server: %w", err)
		}

		guardKey, relayKey, exitKey, err := SetupCircuitWithHops(client, circuitID, circuit, hops)
		if err != nil {
			lastErr = fmt.Errorf("establish session keys: %w", err)
			continue
		}

		relayAddr, exitAddr := circuitHopAddresses(circuit, hops)
		payload, err := BuildOnionWithHops(guardKey, relayKey, exitKey, exitLayer, relayAddr, exitAddr, hops)
		if err != nil {
			return nil, fmt.Errorf("build onion payload: %w", err)
		}

		guardURL := fmt.Sprintf("http://%s:%d", circuit.Guard.Host, circuit.Guard.Port)
		onionResp, err := SendOnion(client, guardURL, circuitID, payload)
		if err != nil {
			lastErr = fmt.Errorf("send onion request via guard %s: %w", circuit.Guard.NodeID, err)
			continue
		}

		exitResp, err := DecryptResponseWithHops(guardKey, relayKey, exitKey, onionResp.Payload, hops)
		if err != nil {
			return nil, fmt.Errorf("decrypt circuit response: %w", err)
		}

		return exitResp, nil
	}

	return nil, fmt.Errorf("all %d circuit attempts failed: %w", maxCircuitAttempts, lastErr)
}

// GetCircuit calls the directory server's GET /circuit endpoint and returns the circuit.
func GetCircuit(client *http.Client, directoryURL string) (*CircuitResponse, error) {
	return GetCircuitWithHops(client, directoryURL, 3)
}

// GetCircuitWithHops fetches a circuit from the directory server for the requested hop count.
func GetCircuitWithHops(client *http.Client, directoryURL string, hops int) (*CircuitResponse, error) {
	if err := validateHops(hops); err != nil {
		return nil, err
	}

	circuitURL := fmt.Sprintf("%s/circuit?hops=%d", directoryURL, hops)
	resp, err := client.Get(circuitURL)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", circuitURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpStatusError("GET "+circuitURL, resp)
	}
	var circuit CircuitResponse
	if err := json.NewDecoder(resp.Body).Decode(&circuit); err != nil {
		return nil, fmt.Errorf("decode circuit response: %w", err)
	}
	if circuit.Guard.NodeID == "" {
		return nil, fmt.Errorf("decode circuit response: guard node missing")
	}
	if hops == 3 && (circuit.Relay.NodeID == "" || circuit.Exit.NodeID == "") {
		return nil, fmt.Errorf("decode circuit response: relay or exit node missing for 3-hop circuit")
	}
	return &circuit, nil
}

// SetupCircuit generates a fresh 32-byte AES-256 session key for each hop, RSA-OAEP
// encrypts it with the node's public key, and POST /setup to each node in the circuit.
// Returns (guardKey, relayKey, exitKey) for use with BuildOnion and DecryptResponse.
func SetupCircuit(client *http.Client, circuitID string, circuit *CircuitResponse) (guardKey, relayKey, exitKey []byte, err error) {
	return SetupCircuitWithHops(client, circuitID, circuit, 3)
}

// SetupCircuitWithHops establishes session keys for the requested hop count.
func SetupCircuitWithHops(client *http.Client, circuitID string, circuit *CircuitResponse, hops int) (guardKey, relayKey, exitKey []byte, err error) {
	if err := validateHops(hops); err != nil {
		return nil, nil, nil, err
	}

	guardKey = make([]byte, 32)
	if _, err = rand.Read(guardKey); err != nil {
		return nil, nil, nil, fmt.Errorf("generate session key: %w", err)
	}

	candidates := []struct {
		node NodeRecord
		key  []byte
	}{
		{circuit.Guard, guardKey},
	}
	if hops == 3 {
		relayKey = make([]byte, 32)
		exitKey = make([]byte, 32)
		for _, k := range [][]byte{relayKey, exitKey} {
			if _, err = rand.Read(k); err != nil {
				return nil, nil, nil, fmt.Errorf("generate session key: %w", err)
			}
		}
		candidates = append(candidates,
			struct {
				node NodeRecord
				key  []byte
			}{circuit.Relay, relayKey},
			struct {
				node NodeRecord
				key  []byte
			}{circuit.Exit, exitKey},
		)
	}

	for _, tc := range candidates {
		pub, parseErr := onion.ParsePublicKey(tc.node.PublicKey)
		if parseErr != nil {
			return nil, nil, nil, fmt.Errorf("parse public key for node %s: %w", tc.node.NodeID, parseErr)
		}
		nodeURL := fmt.Sprintf("http://%s:%d", tc.node.Host, tc.node.Port)
		if sendErr := sendSetupKey(client, nodeURL, circuitID, pub, tc.key); sendErr != nil {
			return nil, nil, nil, fmt.Errorf("setup key for node %s: %w", tc.node.NodeID, sendErr)
		}
	}

	return guardKey, relayKey, exitKey, nil
}

// sendSetupKey RSA-OAEP encrypts key with pub and POSTs it to nodeURL/setup.
func sendSetupKey(client *http.Client, nodeURL, circuitID string, pub *rsa.PublicKey, key []byte) error {
	ct, err := onion.EncryptKey(pub, key)
	if err != nil {
		return fmt.Errorf("encrypt key: %w", err)
	}
	body, err := json.Marshal(onion.KeySetupRequest{
		CircuitID:    circuitID,
		EncryptedKey: base64.StdEncoding.EncodeToString(ct),
	})
	if err != nil {
		return fmt.Errorf("marshal setup request: %w", err)
	}
	resp, err := client.Post(nodeURL+"/setup", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST /setup to %s: %w", nodeURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return httpStatusError("POST /setup to "+nodeURL, resp)
	}
	return nil
}

// BuildOnion constructs a 3-layer onion-encrypted payload for a circuit.
//
// Layers (inside-out):
//
//	exit layer  : ExitLayer  encrypted with exitKey  → only exit node sees the URL
//	relay layer : Layer{nextHop=exitAddr}  encrypted with relayKey
//	guard layer : Layer{nextHop=relayAddr} encrypted with guardKey  ← sent to guard
func BuildOnion(guardKey, relayKey, exitKey []byte, exitLayer onion.ExitLayer, relayAddr, exitAddr string) ([]byte, error) {
	return BuildOnionWithHops(guardKey, relayKey, exitKey, exitLayer, relayAddr, exitAddr, 3)
}

// BuildOnionWithHops constructs a hop-count-specific onion payload.
func BuildOnionWithHops(guardKey, relayKey, exitKey []byte, exitLayer onion.ExitLayer, relayAddr, exitAddr string, hops int) ([]byte, error) {
	if err := validateHops(hops); err != nil {
		return nil, err
	}

	exitLayerJSON, err := json.Marshal(exitLayer)
	if err != nil {
		return nil, fmt.Errorf("marshal exit layer: %w", err)
	}
	layerKey := exitKey
	if hops == 1 {
		layerKey = guardKey
	}
	exitCT, err := onion.Encrypt(layerKey, exitLayerJSON)
	if err != nil {
		return nil, fmt.Errorf("encrypt exit layer: %w", err)
	}
	if hops == 1 {
		return exitCT, nil
	}

	relayLayerJSON, err := json.Marshal(onion.Layer{
		NextHop: exitAddr,
		Payload: exitCT,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal relay layer: %w", err)
	}
	relayCT, err := onion.Encrypt(relayKey, relayLayerJSON)
	if err != nil {
		return nil, fmt.Errorf("encrypt relay layer: %w", err)
	}

	guardLayerJSON, err := json.Marshal(onion.Layer{
		NextHop: relayAddr,
		Payload: relayCT,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal guard layer: %w", err)
	}
	guardCT, err := onion.Encrypt(guardKey, guardLayerJSON)
	if err != nil {
		return nil, fmt.Errorf("encrypt guard layer: %w", err)
	}
	return guardCT, nil
}

// DecryptResponse peels all 3 onion layers from the circuit response.
//
// Return-path encryption order (each node wraps what it received):
//
//	exit  → AES-GCM-encrypt(exitKey,  ExitResponse JSON)
//	relay → AES-GCM-encrypt(relayKey, exit-encrypted bytes)
//	guard → AES-GCM-encrypt(guardKey, relay-encrypted bytes)
//
// Client peels: guardKey → relayKey → exitKey → ExitResponse.
func DecryptResponse(guardKey, relayKey, exitKey, payload []byte) (*onion.ExitResponse, error) {
	return DecryptResponseWithHops(guardKey, relayKey, exitKey, payload, 3)
}

// DecryptResponseWithHops peels the onion response according to the requested hop count.
func DecryptResponseWithHops(guardKey, relayKey, exitKey, payload []byte, hops int) (*onion.ExitResponse, error) {
	if err := validateHops(hops); err != nil {
		return nil, err
	}

	if hops == 1 {
		exitRespJSON, err := onion.Decrypt(guardKey, payload)
		if err != nil {
			return nil, fmt.Errorf("decrypt guard layer: %w", err)
		}
		var exitResp onion.ExitResponse
		if err := json.Unmarshal(exitRespJSON, &exitResp); err != nil {
			return nil, fmt.Errorf("unmarshal ExitResponse: %w", err)
		}
		return &exitResp, nil
	}

	relayEncrypted, err := onion.Decrypt(guardKey, payload)
	if err != nil {
		return nil, fmt.Errorf("decrypt guard layer: %w", err)
	}
	exitEncrypted, err := onion.Decrypt(relayKey, relayEncrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt relay layer: %w", err)
	}
	exitRespJSON, err := onion.Decrypt(exitKey, exitEncrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt exit layer: %w", err)
	}
	var exitResp onion.ExitResponse
	if err := json.Unmarshal(exitRespJSON, &exitResp); err != nil {
		return nil, fmt.Errorf("unmarshal ExitResponse: %w", err)
	}
	return &exitResp, nil
}

// RegisterKey sends the AES-256 session key for a circuit to a node's POST /key endpoint.
func RegisterKey(client *http.Client, nodeURL, circuitID string, key []byte) error {
	body, err := json.Marshal(onion.KeyRequest{
		CircuitID: circuitID,
		Key:       base64.StdEncoding.EncodeToString(key),
	})
	if err != nil {
		return fmt.Errorf("marshal key request: %w", err)
	}
	resp, err := client.Post(nodeURL+"/key", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST /key to %s: %w", nodeURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return httpStatusError("POST /key to "+nodeURL, resp)
	}
	return nil
}

// SendOnion sends the onion payload to the guard's POST /onion endpoint and returns the response.
func SendOnion(client *http.Client, guardURL, circuitID string, payload []byte) (*onion.OnionResponse, error) {
	body, err := json.Marshal(onion.OnionRequest{
		CircuitID: circuitID,
		Payload:   payload,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal onion request: %w", err)
	}
	resp, err := client.Post(guardURL+"/onion", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("POST /onion to %s: %w", guardURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpStatusError("POST /onion to "+guardURL, resp)
	}
	var onionResp onion.OnionResponse
	if err := json.NewDecoder(resp.Body).Decode(&onionResp); err != nil {
		return nil, fmt.Errorf("decode onion response: %w", err)
	}
	return &onionResp, nil
}

func validateHops(hops int) error {
	switch hops {
	case 1, 3:
		return nil
	default:
		return fmt.Errorf("hops must be 1 or 3")
	}
}

func circuitHopAddresses(circuit *CircuitResponse, hops int) (relayAddr, exitAddr string) {
	if hops != 3 {
		return "", ""
	}
	return fmt.Sprintf("%s:%d", circuit.Relay.Host, circuit.Relay.Port),
		fmt.Sprintf("%s:%d", circuit.Exit.Host, circuit.Exit.Port)
}

func httpStatusError(action string, resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	message := strings.TrimSpace(string(body))
	if message == "" {
		return fmt.Errorf("%s returned %d", action, resp.StatusCode)
	}
	return fmt.Errorf("%s returned %d: %s", action, resp.StatusCode, message)
}

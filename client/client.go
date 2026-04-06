package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	onion "github.com/rahul-srsh/Torr-BSDS/shared/onion"
)

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

// GetCircuit calls the directory server's GET /circuit endpoint and returns the circuit.
func GetCircuit(client *http.Client, directoryURL string) (*CircuitResponse, error) {
	resp, err := client.Get(directoryURL + "/circuit")
	if err != nil {
		return nil, fmt.Errorf("GET /circuit: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /circuit returned %d", resp.StatusCode)
	}
	var circuit CircuitResponse
	if err := json.NewDecoder(resp.Body).Decode(&circuit); err != nil {
		return nil, fmt.Errorf("decode circuit response: %w", err)
	}
	return &circuit, nil
}

// SetupCircuit generates a fresh 32-byte AES-256 session key for each hop, RSA-OAEP
// encrypts it with the node's public key, and POST /setup to each node in the circuit.
// Returns (guardKey, relayKey, exitKey) for use with BuildOnion and DecryptResponse.
func SetupCircuit(client *http.Client, circuitID string, circuit *CircuitResponse) (guardKey, relayKey, exitKey []byte, err error) {
	guardKey, relayKey, exitKey = make([]byte, 32), make([]byte, 32), make([]byte, 32)
	for _, k := range [][]byte{guardKey, relayKey, exitKey} {
		if _, err = rand.Read(k); err != nil {
			return nil, nil, nil, fmt.Errorf("generate session key: %w", err)
		}
	}

	for _, tc := range []struct {
		node NodeRecord
		key  []byte
	}{
		{circuit.Guard, guardKey},
		{circuit.Relay, relayKey},
		{circuit.Exit, exitKey},
	} {
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
		return fmt.Errorf("POST /setup to %s returned %d", nodeURL, resp.StatusCode)
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
	exitLayerJSON, err := json.Marshal(exitLayer)
	if err != nil {
		return nil, fmt.Errorf("marshal exit layer: %w", err)
	}
	exitCT, err := onion.Encrypt(exitKey, exitLayerJSON)
	if err != nil {
		return nil, fmt.Errorf("encrypt exit layer: %w", err)
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
		return fmt.Errorf("POST /key to %s returned %d", nodeURL, resp.StatusCode)
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
		return nil, fmt.Errorf("POST /onion: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("POST /onion returned %d", resp.StatusCode)
	}
	var onionResp onion.OnionResponse
	if err := json.NewDecoder(resp.Body).Decode(&onionResp); err != nil {
		return nil, fmt.Errorf("decode onion response: %w", err)
	}
	return &onionResp, nil
}

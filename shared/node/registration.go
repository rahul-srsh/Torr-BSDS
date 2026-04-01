// Package node provides startup registration and heartbeat logic shared by
// guard, relay, and exit nodes.
package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// RegisterRequest matches the directory server's POST /register payload.
type RegisterRequest struct {
	NodeID    string `json:"nodeId"`
	NodeType  string `json:"nodeType"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	PublicKey string `json:"publicKey"`
}

// Config holds everything a node needs to register and send heartbeats.
type Config struct {
	NodeID            string
	NodeType          string
	Host              string
	Port              int
	PublicKeyPEM      string
	PrivateKey        *rsa.PrivateKey // retained for Task 6 key exchange
	DirectoryURL      string
	HTTPClient        *http.Client
	HeartbeatInterval time.Duration // defaults to 10s; override in tests
}

// GenerateNodeID returns a random UUID v4 string.
func GenerateNodeID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand.Read: %w", err)
	}
	// Set version 4 and RFC 4122 variant bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// GenerateKeyPair creates a new RSA-2048 key pair and returns the private key
// and the PKIX PEM-encoded public key string.
func GenerateKeyPair() (*rsa.PrivateKey, string, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, "", fmt.Errorf("generate RSA key: %w", err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, "", fmt.Errorf("marshal public key: %w", err)
	}
	pubPEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubBytes,
	}))
	return priv, pubPEM, nil
}

// ecsTaskMeta is the subset of the ECS task metadata v3/v4 response we need.
type ecsTaskMeta struct {
	Containers []struct {
		Networks []struct {
			IPv4Addresses []string `json:"IPv4Addresses"`
		} `json:"Networks"`
	} `json:"Containers"`
}

// ResolveOwnAddress attempts to discover the node's publicly routable IP address.
// It first queries checkip.amazonaws.com to get the public IP (works for Fargate
// tasks in public subnets with assign_public_ip=true), then falls back to the
// ECS task metadata private IP, then os.Hostname().
func ResolveOwnAddress(client *http.Client) (string, error) {
	// Try public IP first so clients outside the VPC can reach this node.
	if ip, err := publicIPFromCheckIP(client); err == nil && ip != "" {
		return ip, nil
	}
	for _, envVar := range []string{"ECS_CONTAINER_METADATA_URI_V4", "ECS_CONTAINER_METADATA_URI"} {
		uri := os.Getenv(envVar)
		if uri == "" {
			continue
		}
		ip, err := ipFromECSMetadata(client, uri+"/task")
		if err == nil && ip != "" {
			return ip, nil
		}
		log.Printf("[node] ECS metadata via %s failed: %v", envVar, err)
	}
	// Fallback: use hostname (works for local dev and Docker)
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("hostname: %w", err)
	}
	return hostname, nil
}

// publicIPFromCheckIP queries checkip.amazonaws.com and returns the public IP
// of the caller. Returns an error if the request fails or times out.
func publicIPFromCheckIP(client *http.Client) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://checkip.amazonaws.com", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if ip == "" {
		return "", fmt.Errorf("empty response from checkip")
	}
	return ip, nil
}

func ipFromECSMetadata(client *http.Client, url string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var meta ecsTaskMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		return "", fmt.Errorf("parse ECS metadata: %w", err)
	}
	for _, c := range meta.Containers {
		for _, n := range c.Networks {
			if len(n.IPv4Addresses) > 0 {
				return n.IPv4Addresses[0], nil
			}
		}
	}
	return "", fmt.Errorf("no IPv4 address in ECS metadata")
}

// Register calls POST /register on the directory server once.
func Register(ctx context.Context, cfg *Config) error {
	body, err := json.Marshal(RegisterRequest{
		NodeID:    cfg.NodeID,
		NodeType:  cfg.NodeType,
		Host:      cfg.Host,
		Port:      cfg.Port,
		PublicKey: cfg.PublicKeyPEM,
	})
	if err != nil {
		return fmt.Errorf("marshal register request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.DirectoryURL+"/register", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST /register: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST /register returned %d: %s", resp.StatusCode, body)
	}
	return nil
}

// sendHeartbeat sends a single POST /heartbeat/{nodeId} to the directory server.
func sendHeartbeat(ctx context.Context, cfg *Config) error {
	url := cfg.DirectoryURL + "/heartbeat/" + cfg.NodeID
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("build heartbeat request: %w", err)
	}
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST /heartbeat: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST /heartbeat returned %d", resp.StatusCode)
	}
	return nil
}

// StartHeartbeat launches a background goroutine that sends a heartbeat to the
// directory server every HeartbeatInterval (default 10s).
func StartHeartbeat(ctx context.Context, cfg *Config) {
	interval := cfg.HeartbeatInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := sendHeartbeat(ctx, cfg); err != nil {
					log.Printf("[%s] heartbeat failed: %v", cfg.NodeType, err)
				} else {
					log.Printf("[%s] heartbeat sent (nodeId=%s)", cfg.NodeType, cfg.NodeID)
				}
			}
		}
	}()
}

// StartWithBackoff registers with the directory server using exponential backoff
// (1s → 2s → 4s … capped at 60s). Once registered it starts the heartbeat
// goroutine. This is meant to be called as a goroutine so it does not block startup.
func StartWithBackoff(ctx context.Context, cfg *Config) {
	backoff := time.Second
	const maxBackoff = 60 * time.Second
	for {
		err := Register(ctx, cfg)
		if err == nil {
			log.Printf("[%s] registered with directory server (nodeId=%s host=%s port=%d)",
				cfg.NodeType, cfg.NodeID, cfg.Host, cfg.Port)
			StartHeartbeat(ctx, cfg)
			return
		}
		log.Printf("[%s] registration failed: %v — retrying in %s", cfg.NodeType, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

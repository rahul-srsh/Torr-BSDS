package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rahul-srsh/Torr-BSDS/shared/config"
	"github.com/rahul-srsh/Torr-BSDS/shared/node"
	onion "github.com/rahul-srsh/Torr-BSDS/shared/onion"
	sharedserver "github.com/rahul-srsh/Torr-BSDS/shared/server"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

func main() {
	cfg := config.Load()

	// ── Node self-registration ────────────────────────────────────────────────
	nodeID, err := node.GenerateNodeID()
	if err != nil {
		log.Fatalf("[guard] generate node ID: %v", err)
	}
	privKey, pubKeyPEM, err := node.GenerateKeyPair()
	if err != nil {
		log.Fatalf("[guard] generate key pair: %v", err)
	}
	host, err := node.ResolveOwnAddress(httpClient)
	if err != nil {
		log.Printf("[guard] resolve address: %v — continuing with empty host", err)
	}
	port, _ := strconv.Atoi(cfg.Port)

	regCfg := &node.Config{
		NodeID:       nodeID,
		NodeType:     cfg.NodeType,
		Host:         host,
		Port:         port,
		PublicKeyPEM: pubKeyPEM,
		PrivateKey:   privKey,
		DirectoryURL: cfg.DirectoryServerURL,
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
	}
	go node.StartWithBackoff(context.Background(), regCfg)

	// ── HTTP routes ───────────────────────────────────────────────────────────
	srv := sharedserver.New(cfg)

	targetURL := strings.TrimRight(os.Getenv("FORWARD_TARGET_URL"), "/")
	srv.Mux.HandleFunc("/forward/echo", forwardEchoHandler(targetURL, httpClient))

	keys := onion.NewKeyStore()
	h := onion.NewHandlerWithDirectExit(keys, httpClient, "guard")
	srv.Mux.HandleFunc("/key", h.HandleKey)
	srv.Mux.HandleFunc("/onion", h.HandleOnion)
	srv.Mux.HandleFunc("/setup", onion.HandleSetup(keys, privKey))

	srv.Start()
}

func forwardEchoHandler(targetBaseURL string, client *http.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if targetBaseURL == "" {
			http.Error(w, "FORWARD_TARGET_URL is not configured", http.StatusInternalServerError)
			return
		}

		targetURL := targetBaseURL + "/echo"
		if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}

		req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
		if err != nil {
			http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
			return
		}

		for key, values := range r.Header {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}

		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "failed to call echo server", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}

		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

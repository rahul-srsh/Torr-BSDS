package main

import (
	"context"
	"log"
	"net/http"
	"strconv"
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
		log.Fatalf("[relay] generate node ID: %v", err)
	}
	privKey, pubKeyPEM, err := node.GenerateKeyPair()
	if err != nil {
		log.Fatalf("[relay] generate key pair: %v", err)
	}
	host, err := node.ResolveOwnAddress(httpClient)
	if err != nil {
		log.Printf("[relay] resolve address: %v — continuing with empty host", err)
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

	keys := onion.NewKeyStore()
	h := onion.NewHandler(keys, httpClient, "relay")
	srv.Mux.HandleFunc("/key", h.HandleKey)
	srv.Mux.HandleFunc("/onion", h.HandleOnion)
	srv.Mux.HandleFunc("/setup", onion.HandleSetup(keys, privKey))

	srv.Start()
}

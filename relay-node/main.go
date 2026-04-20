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

var (
	loadConfig        = config.Load
	newIdentity       = defaultNewIdentity
	startServer       = defaultStartServer
	startRegistration = defaultStartRegistration
)

func defaultNewIdentity() (*node.Identity, error) { return node.NewIdentity(httpClient) }
func defaultStartServer(s *sharedserver.BaseServer) { s.Start() }
func defaultStartRegistration(ctx context.Context, cfg *node.Config) {
	go node.StartWithBackoff(ctx, cfg)
}

func run() error {
	cfg := loadConfig()

	identity, err := newIdentity()
	if err != nil {
		return err
	}
	port, _ := strconv.Atoi(cfg.Port)

	regCfg := &node.Config{
		NodeID:       identity.NodeID,
		NodeType:     cfg.NodeType,
		Host:         identity.Host,
		Port:         port,
		PublicKeyPEM: identity.PublicKeyPEM,
		PrivateKey:   identity.PrivateKey,
		DirectoryURL: cfg.DirectoryServerURL,
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
	}
	startRegistration(context.Background(), regCfg)

	srv := sharedserver.New(cfg)

	keys := onion.NewKeyStore()
	h := onion.NewHandler(keys, httpClient, "relay")
	srv.Mux.HandleFunc("/key", h.HandleKey)
	srv.Mux.HandleFunc("/onion", h.HandleOnion)
	srv.Mux.HandleFunc("/setup", onion.HandleSetup(keys, identity.PrivateKey))

	startServer(srv)
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("[relay] %v", err)
	}
}

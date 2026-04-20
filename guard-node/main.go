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

var (
	loadConfig        = config.Load
	newIdentity       = defaultNewIdentity
	startServer       = defaultStartServer
	startRegistration = defaultStartRegistration
	getForwardTarget  = defaultGetForwardTarget
)

func defaultNewIdentity() (*node.Identity, error) { return node.NewIdentity(httpClient) }
func defaultStartServer(s *sharedserver.BaseServer) { s.Start() }
func defaultStartRegistration(ctx context.Context, cfg *node.Config) {
	go node.StartWithBackoff(ctx, cfg)
}
func defaultGetForwardTarget() string {
	return strings.TrimRight(os.Getenv("FORWARD_TARGET_URL"), "/")
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

	srv.Mux.HandleFunc("/forward/echo", forwardEchoHandler(getForwardTarget(), httpClient))

	keys := onion.NewKeyStore()
	h := onion.NewHandlerWithDirectExit(keys, httpClient, "guard")
	srv.Mux.HandleFunc("/key", h.HandleKey)
	srv.Mux.HandleFunc("/onion", h.HandleOnion)
	srv.Mux.HandleFunc("/setup", onion.HandleSetup(keys, identity.PrivateKey))

	startServer(srv)
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("[guard] %v", err)
	}
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

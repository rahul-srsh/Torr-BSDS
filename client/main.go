package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	onion "github.com/rahul-srsh/Torr-BSDS/shared/onion"
)

type clientConfig struct {
	DirectoryURL   string
	DestinationURL string
	Method         string
	Body           string
	Hops           int
	Timeout        time.Duration
}

func parseClientConfig(args []string) (*clientConfig, error) {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	cfg := &clientConfig{}
	fs.StringVar(&cfg.DirectoryURL, "directory-url", "", "directory server base URL")
	fs.StringVar(&cfg.DestinationURL, "destination-url", "", "destination URL to request")
	fs.StringVar(&cfg.Method, "method", http.MethodGet, "HTTP method")
	fs.StringVar(&cfg.Body, "body", "", "request body")
	fs.IntVar(&cfg.Hops, "hops", 3, "hop count (1 or 3)")
	fs.DurationVar(&cfg.Timeout, "timeout", 15*time.Second, "HTTP client timeout")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if cfg.DirectoryURL == "" {
		return nil, fmt.Errorf("directory-url is required")
	}
	if cfg.DestinationURL == "" {
		return nil, fmt.Errorf("destination-url is required")
	}
	if err := validateHops(cfg.Hops); err != nil {
		return nil, err
	}

	cfg.Method = strings.ToUpper(strings.TrimSpace(cfg.Method))
	if cfg.Method == "" {
		cfg.Method = http.MethodGet
	}

	return cfg, nil
}

func runClient(cfg *clientConfig, stdout io.Writer) error {
	client := &http.Client{Timeout: cfg.Timeout}
	circuitID := fmt.Sprintf("client-%d", time.Now().UnixNano())

	circuit, err := GetCircuitWithHops(client, cfg.DirectoryURL, cfg.Hops)
	if err != nil {
		return err
	}

	guardKey, relayKey, exitKey, err := SetupCircuitWithHops(client, circuitID, circuit, cfg.Hops)
	if err != nil {
		return err
	}

	exitLayer := onion.ExitLayer{
		URL:    cfg.DestinationURL,
		Method: cfg.Method,
	}
	if cfg.Body != "" {
		exitLayer.Body = []byte(cfg.Body)
	}

	var relayAddr, exitAddr string
	if cfg.Hops == 3 {
		relayAddr = fmt.Sprintf("%s:%d", circuit.Relay.Host, circuit.Relay.Port)
		exitAddr = fmt.Sprintf("%s:%d", circuit.Exit.Host, circuit.Exit.Port)
	}

	payload, err := BuildOnionWithHops(guardKey, relayKey, exitKey, exitLayer, relayAddr, exitAddr, cfg.Hops)
	if err != nil {
		return err
	}

	guardURL := fmt.Sprintf("http://%s:%d", circuit.Guard.Host, circuit.Guard.Port)
	onionResp, err := SendOnion(client, guardURL, circuitID, payload)
	if err != nil {
		return err
	}

	exitResp, err := DecryptResponseWithHops(guardKey, relayKey, exitKey, onionResp.Payload, cfg.Hops)
	if err != nil {
		return err
	}

	if _, err := stdout.Write(exitResp.Body); err != nil {
		return fmt.Errorf("write response body: %w", err)
	}

	return nil
}

func main() {
	cfg, err := parseClientConfig(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	if err := runClient(cfg, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

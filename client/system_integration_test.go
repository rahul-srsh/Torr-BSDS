package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rahul-srsh/Torr-BSDS/shared/node"
	onion "github.com/rahul-srsh/Torr-BSDS/shared/onion"
)

type groupedNodesResponse struct {
	Guard []NodeRecord `json:"guard"`
	Relay []NodeRecord `json:"relay"`
	Exit  []NodeRecord `json:"exit"`
}

type systemNode struct {
	record NodeRecord
	server *httptest.Server
	cancel context.CancelFunc
}

type systemHarness struct {
	directoryURL string
	echoURL      string
	client       *http.Client
	guard        *systemNode
	relay        *systemNode
	exit         *systemNode
	nodeLogs     *bytes.Buffer
	stopDir      func()
	stopEcho     func()
	restoreLog   func()
}

func TestSystemIntegrationFullFlow(t *testing.T) {
	h := newSystemHarness(t)

	t.Run("three-hop-flow", func(t *testing.T) {
		destinationURL := h.echoURL + "/echo?mode=three-hop"
		resp, err := ExecuteRequestWithHops(h.client, h.directoryURL, "system-3hop", onion.ExitLayer{
			URL:     destinationURL,
			Method:  http.MethodPost,
			Headers: map[string]string{"Content-Type": "text/plain"},
			Body:    []byte("three-hop-body"),
		}, 3)
		if err != nil {
			t.Fatalf("ExecuteRequestWithHops(3): %v", err)
		}

		verifyEchoResponse(t, resp, http.MethodPost, "/echo", "three-hop", "three-hop-body")
	})

	t.Run("one-hop-flow", func(t *testing.T) {
		destinationURL := h.echoURL + "/echo?mode=one-hop"
		resp, err := ExecuteRequestWithHops(h.client, h.directoryURL, "system-1hop", onion.ExitLayer{
			URL:    destinationURL,
			Method: http.MethodGet,
		}, 1)
		if err != nil {
			t.Fatalf("ExecuteRequestWithHops(1): %v", err)
		}

		verifyEchoResponse(t, resp, http.MethodGet, "/echo", "one-hop", "")
	})

	t.Run("five-concurrent-circuits", func(t *testing.T) {
		type result struct {
			mode string
			body string
			err  error
		}

		results := make(chan result, 5)
		var wg sync.WaitGroup

		for i := 0; i < 5; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()

				requestBody := fmt.Sprintf("circuit-%d", i)
				resp, err := ExecuteRequestWithHops(h.client, h.directoryURL, fmt.Sprintf("concurrent-%d", i), onion.ExitLayer{
					URL:     fmt.Sprintf("%s/echo?mode=concurrent-%d", h.echoURL, i),
					Method:  http.MethodPost,
					Headers: map[string]string{"Content-Type": "text/plain"},
					Body:    []byte(requestBody),
				}, 3)
				if err != nil {
					results <- result{err: err}
					return
				}

				var payload struct {
					Query map[string]string `json:"query"`
					Body  string            `json:"body"`
				}
				if err := json.Unmarshal(resp.Body, &payload); err != nil {
					results <- result{err: err}
					return
				}

				results <- result{
					mode: payload.Query["mode"],
					body: payload.Body,
				}
			}()
		}

		wg.Wait()
		close(results)

		seenModes := make(map[string]bool, 5)
		for result := range results {
			if result.err != nil {
				t.Fatalf("concurrent circuit failed: %v", result.err)
			}
			if seenModes[result.mode] {
				t.Fatalf("duplicate mode %q indicates response mixup", result.mode)
			}
			seenModes[result.mode] = true
			if want := strings.TrimPrefix(result.mode, "concurrent-"); result.body != "circuit-"+want {
				t.Fatalf("body for %s = %q, want %q", result.mode, result.body, "circuit-"+want)
			}
		}

		if len(seenModes) != 5 {
			t.Fatalf("saw %d concurrent responses, want 5", len(seenModes))
		}
	})

	t.Run("privacy-logs", func(t *testing.T) {
		offset := h.nodeLogs.Len()
		destinationURL := h.echoURL + "/echo?mode=privacy"

		resp, err := ExecuteRequestWithHops(h.client, h.directoryURL, "privacy-check", onion.ExitLayer{
			URL:    destinationURL,
			Method: http.MethodGet,
		}, 3)
		if err != nil {
			t.Fatalf("ExecuteRequestWithHops(privacy): %v", err)
		}
		verifyEchoResponse(t, resp, http.MethodGet, "/echo", "privacy", "")

		logs := h.nodeLogs.String()[offset:]
		guardLine := findLogLine(logs, "[guard] circuit privacy-check:")
		relayLine := findLogLine(logs, "[relay] circuit privacy-check:")
		exitLine := findLogLine(logs, "[exit] circuit privacy-check")

		if guardLine == "" || relayLine == "" || exitLine == "" {
			t.Fatalf("missing privacy logs:\n%s", logs)
		}

		relayAddr := addr(h.relay.server)
		exitAddr := addr(h.exit.server)

		if !strings.Contains(guardLine, relayAddr) {
			t.Fatalf("guard log = %q, want relay address %q", guardLine, relayAddr)
		}
		if strings.Contains(guardLine, destinationURL) {
			t.Fatalf("guard log leaks destination: %q", guardLine)
		}

		if !strings.Contains(relayLine, exitAddr) {
			t.Fatalf("relay log = %q, want exit address %q", relayLine, exitAddr)
		}
		if strings.Contains(relayLine, destinationURL) {
			t.Fatalf("relay log leaks destination: %q", relayLine)
		}

		if !strings.Contains(exitLine, destinationURL) {
			t.Fatalf("exit log = %q, want destination %q", exitLine, destinationURL)
		}
		clientAddr := extractPreviousHop(guardLine)
		if clientAddr == "" {
			t.Fatalf("could not extract client address from guard log: %q", guardLine)
		}
		if strings.Contains(exitLine, clientAddr) {
			t.Fatalf("exit log should not contain the client address %q: %q", clientAddr, exitLine)
		}
	})
}

func newSystemHarness(t *testing.T) *systemHarness {
	t.Helper()

	origWriter := log.Writer()
	nodeLogs := &bytes.Buffer{}
	log.SetOutput(nodeLogs)

	restoreLog := func() {
		log.SetOutput(origWriter)
	}
	t.Cleanup(restoreLog)

	directoryPort := reserveTCPPort(t)
	directoryURL := fmt.Sprintf("http://127.0.0.1:%d", directoryPort)
	_, stopDir := startGoService(t, filepath.Join("..", "directory-server"), directoryPort, map[string]string{
		"NODE_TYPE":            "directory-server",
		"DIRECTORY_SERVER_URL": directoryURL, // required by config.Load; unused by the directory server itself
	})
	t.Cleanup(stopDir)

	echoPort := reserveTCPPort(t)
	echoURL, stopEcho := startGoService(t, filepath.Join("..", "echo-server"), echoPort, map[string]string{
		"NODE_TYPE":            "echo-server",
		"DIRECTORY_SERVER_URL": directoryURL, // required by config.Load; unused by the echo server itself
	})
	t.Cleanup(stopEcho)

	h := &systemHarness{
		directoryURL: directoryURL,
		echoURL:      echoURL,
		client:       &http.Client{Timeout: 3 * time.Second},
		nodeLogs:     nodeLogs,
		stopDir:      stopDir,
		stopEcho:     stopEcho,
		restoreLog:   restoreLog,
	}

	h.guard = startRegisteredNode(t, directoryURL, "guard", true)
	h.relay = startRegisteredNode(t, directoryURL, "relay", false)
	h.exit = startRegisteredNode(t, directoryURL, "exit", false)

	t.Cleanup(func() {
		h.guard.cancel()
		h.relay.cancel()
		h.exit.cancel()
		h.guard.server.Close()
		h.relay.server.Close()
		h.exit.server.Close()
	})

	waitForHealthyNodes(t, h.client, directoryURL, 1, 1, 1)

	return h
}

func startRegisteredNode(t *testing.T, directoryURL, nodeType string, directExit bool) *systemNode {
	t.Helper()

	privKey, pubKeyPEM := genIntegrationKeyPair(t)
	keys := onion.NewKeyStore()
	mux := http.NewServeMux()

	switch nodeType {
	case "guard":
		handler := onion.NewHandlerWithDirectExit(keys, http.DefaultClient, nodeType)
		if !directExit {
			handler = onion.NewHandler(keys, http.DefaultClient, nodeType)
		}
		mux.HandleFunc("/key", handler.HandleKey)
		mux.HandleFunc("/onion", handler.HandleOnion)
	case "relay":
		handler := onion.NewHandler(keys, http.DefaultClient, nodeType)
		mux.HandleFunc("/key", handler.HandleKey)
		mux.HandleFunc("/onion", handler.HandleOnion)
	case "exit":
		handler := onion.NewExitHandler(keys, http.DefaultClient)
		mux.HandleFunc("/key", handler.HandleKey)
		mux.HandleFunc("/onion", handler.HandleOnion)
	default:
		t.Fatalf("unsupported node type %q", nodeType)
	}
	mux.HandleFunc("/setup", onion.HandleSetup(keys, privKey))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := httptest.NewServer(mux)
	record := nodeRecordFromURL(t, mustNodeID(t), nodeType, server.URL, pubKeyPEM)

	ctx, cancel := context.WithCancel(context.Background())
	cfg := &node.Config{
		NodeID:            record.NodeID,
		NodeType:          nodeType,
		Host:              record.Host,
		Port:              record.Port,
		PublicKeyPEM:      pubKeyPEM,
		PrivateKey:        privKey,
		DirectoryURL:      directoryURL,
		HTTPClient:        &http.Client{Timeout: 2 * time.Second},
		HeartbeatInterval: 250 * time.Millisecond,
	}

	registerCtx, registerCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer registerCancel()
	if err := node.Register(registerCtx, cfg); err != nil {
		server.Close()
		cancel()
		t.Fatalf("register %s node: %v", nodeType, err)
	}
	node.StartHeartbeat(ctx, cfg)

	return &systemNode{
		record: record,
		server: server,
		cancel: cancel,
	}
}

func waitForHealthyNodes(t *testing.T, client *http.Client, directoryURL string, guards, relays, exits int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(directoryURL + "/nodes")
		if err == nil {
			var grouped groupedNodesResponse
			decodeErr := json.NewDecoder(resp.Body).Decode(&grouped)
			resp.Body.Close()
			if decodeErr == nil && len(grouped.Guard) == guards && len(grouped.Relay) == relays && len(grouped.Exit) == exits {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("directory server did not report %d/%d/%d healthy nodes in time", guards, relays, exits)
}

func verifyEchoResponse(t *testing.T, resp *onion.ExitResponse, method, path, mode, body string) {
	t.Helper()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("statusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var payload struct {
		NodeType string            `json:"nodeType"`
		Method   string            `json:"method"`
		Path     string            `json:"path"`
		Query    map[string]string `json:"query"`
		Body     string            `json:"body"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatalf("json.Unmarshal(response): %v", err)
	}

	if payload.NodeType != "echo-server" {
		t.Fatalf("nodeType = %q, want echo-server", payload.NodeType)
	}
	if payload.Method != method {
		t.Fatalf("method = %q, want %q", payload.Method, method)
	}
	if payload.Path != path {
		t.Fatalf("path = %q, want %q", payload.Path, path)
	}
	if payload.Query["mode"] != mode {
		t.Fatalf("query[mode] = %q, want %q", payload.Query["mode"], mode)
	}
	if payload.Body != body {
		t.Fatalf("body = %q, want %q", payload.Body, body)
	}
}

func startGoService(t *testing.T, workdir string, port int, extraEnv map[string]string) (string, func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "go", "run", ".")
	cmd.Dir = workdir

	env := append(os.Environ(), "PORT="+strconv.Itoa(port))
	for key, value := range extraEnv {
		env = append(env, key+"="+value)
	}
	cmd.Env = env

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start %s: %v", workdir, err)
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHealth(t, baseURL, &output)

	stop := func() {
		cancel()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}

	return baseURL, stop
}

func waitForHealth(t *testing.T, baseURL string, output *bytes.Buffer) {
	t.Helper()

	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("service %s did not become healthy:\n%s", baseURL, output.String())
}

func reserveTCPPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer listener.Close()

	return listener.Addr().(*net.TCPAddr).Port
}

func mustNodeID(t *testing.T) string {
	t.Helper()

	nodeID, err := node.GenerateNodeID()
	if err != nil {
		t.Fatalf("node.GenerateNodeID: %v", err)
	}
	return nodeID
}

func findLogLine(logs, needle string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func extractPreviousHop(line string) string {
	const marker = ": "
	idx := strings.Index(line, marker)
	if idx == -1 {
		return ""
	}

	rest := line[idx+len(marker):]
	arrow := strings.Index(rest, " → ")
	if arrow == -1 {
		return ""
	}

	return strings.TrimSpace(rest[:arrow])
}

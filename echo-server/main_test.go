package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rahul-srsh/Torr-BSDS/shared/config"
	sharedserver "github.com/rahul-srsh/Torr-BSDS/shared/server"
)

func TestEchoHandler(t *testing.T) {
	handler := echoHandler("echo-server")

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/echo?scenario=direct", strings.NewReader("ping"))
	handler(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var response echoResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if response.NodeType != "echo-server" {
		t.Fatalf("NodeType = %q, want %q", response.NodeType, "echo-server")
	}
	if response.Method != http.MethodPost {
		t.Fatalf("Method = %q, want %q", response.Method, http.MethodPost)
	}
	if response.Path != "/echo" {
		t.Fatalf("Path = %q, want %q", response.Path, "/echo")
	}
	if response.Query["scenario"] != "direct" {
		t.Fatalf("Query[scenario] = %q, want %q", response.Query["scenario"], "direct")
	}
	if response.Body != "ping" || response.BodyBytes != 4 {
		t.Fatalf("body info = %q/%d, want ping/4", response.Body, response.BodyBytes)
	}
}

func TestEchoHandlerReadError(t *testing.T) {
	handler := echoHandler("echo-server")

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/echo", io.NopCloser(errReader{}))
	handler(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestRunWiresEchoRoute(t *testing.T) {
	origLoad := loadConfig
	origStart := startServer
	t.Cleanup(func() {
		loadConfig = origLoad
		startServer = origStart
	})

	loadConfig = func() *config.NodeConfig {
		return &config.NodeConfig{Port: "0", NodeType: "echo-server"}
	}

	var started *sharedserver.BaseServer
	startServer = func(s *sharedserver.BaseServer) {
		started = s
	}

	run()

	if started == nil {
		t.Fatal("startServer was not called")
	}

	recorder := httptest.NewRecorder()
	started.Mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/echo", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/echo status = %d, want 200", recorder.Code)
	}
}

func TestMainCallsRun(t *testing.T) {
	origLoad := loadConfig
	origStart := startServer
	t.Cleanup(func() {
		loadConfig = origLoad
		startServer = origStart
	})

	called := false
	loadConfig = func() *config.NodeConfig {
		called = true
		return &config.NodeConfig{Port: "0", NodeType: "echo-server"}
	}
	startServer = func(*sharedserver.BaseServer) {}

	main()
	if !called {
		t.Fatal("main did not delegate to run")
	}
}

func TestDefaultStartServerServesHealth(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	listener.Close()

	srv := sharedserver.New(&config.NodeConfig{Port: port, NodeType: "echo-server-default"})
	go defaultStartServer(srv)

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:" + port + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not become healthy")
}

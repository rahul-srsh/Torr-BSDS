package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/rahul-srsh/Torr-BSDS/shared/config"
	"github.com/rahul-srsh/Torr-BSDS/shared/server"
)

func TestRunWiresUpHealthEndpoint(t *testing.T) {
	origLoad := loadConfig
	origStart := startServer
	t.Cleanup(func() {
		loadConfig = origLoad
		startServer = origStart
	})

	loadConfig = func() *config.NodeConfig {
		return &config.NodeConfig{Port: "0", NodeType: "placeholder"}
	}

	var started *server.BaseServer
	startServer = func(s *server.BaseServer) {
		started = s
	}

	run()

	if started == nil {
		t.Fatal("startServer was not called")
	}
	recorder := httptest.NewRecorder()
	started.Mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/health status = %d, want 200", recorder.Code)
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
		return &config.NodeConfig{Port: "0", NodeType: "placeholder"}
	}
	startServer = func(*server.BaseServer) {}

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

	srv := server.New(&config.NodeConfig{Port: port, NodeType: "shared-default"})
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

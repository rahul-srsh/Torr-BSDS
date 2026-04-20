package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/rahul-srsh/Torr-BSDS/shared/config"
)

func TestNewRegistersHealthEndpoint(t *testing.T) {
	cfg := &config.NodeConfig{Port: "0", NodeType: "test"}
	s := New(cfg)

	if s.Config != cfg {
		t.Fatalf("Config = %p, want %p", s.Config, cfg)
	}
	if s.Mux == nil {
		t.Fatal("Mux is nil")
	}

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	s.Mux.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if ct := recorder.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]string
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %q, want ok", body["status"])
	}
	if body["node_type"] != "test" {
		t.Fatalf("node_type = %q, want test", body["node_type"])
	}
}

func TestStartListensAndServes(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	listener.Close()

	cfg := &config.NodeConfig{Port: port, NodeType: "start-test"}
	s := New(cfg)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Start()
	}()

	// Poll /health until the server is up or we time out.
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:" + port + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = nil
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server did not become healthy: %v", lastErr)
}

func TestNewMuxHandlesUnknownPaths(t *testing.T) {
	cfg := &config.NodeConfig{Port: "0", NodeType: "test"}
	s := New(cfg)

	recorder := httptest.NewRecorder()
	s.Mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", recorder.Code)
	}
}

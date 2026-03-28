package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

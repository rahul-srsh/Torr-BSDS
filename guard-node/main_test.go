package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForwardEchoHandlerRequiresTarget(t *testing.T) {
	handler := forwardEchoHandler("", http.DefaultClient)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/forward/echo", nil)
	handler(recorder, req)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
}

func TestForwardEchoHandlerForwardsRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/echo" {
			t.Fatalf("path = %q, want %q", r.URL.Path, "/echo")
		}
		if r.URL.RawQuery != "mode=baseline" {
			t.Fatalf("query = %q, want %q", r.URL.RawQuery, "mode=baseline")
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("io.ReadAll() error = %v", err)
		}
		if string(body) != "hello" {
			t.Fatalf("body = %q, want %q", string(body), "hello")
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	handler := forwardEchoHandler(upstream.URL, upstream.Client())

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/forward/echo?mode=baseline", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "text/plain")
	handler(recorder, req)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusAccepted)
	}
	if got := recorder.Body.String(); got != `{"ok":true}` {
		t.Fatalf("body = %q, want %q", got, `{"ok":true}`)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want %q", got, "application/json")
	}
}

func TestForwardEchoHandlerHandlesUpstreamFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close()

	handler := forwardEchoHandler(url, http.DefaultClient)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/forward/echo", nil)
	handler(recorder, req)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
}

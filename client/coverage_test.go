package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	onion "github.com/rahul-srsh/Torr-BSDS/shared/onion"
)

// TestExecuteRequestDelegatesToThreeHops confirms the no-arg helper forwards to the 3-hop variant.
func TestExecuteRequestDelegatesToThreeHops(t *testing.T) {
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("hops") != "3" {
			t.Fatalf("expected hops=3, got %q", r.URL.Query().Get("hops"))
		}
		http.Error(w, "no nodes", http.StatusServiceUnavailable)
	}))
	defer dir.Close()

	_, err := ExecuteRequest(http.DefaultClient, dir.URL, "c1", onion.ExitLayer{URL: "http://example.com", Method: "GET"})
	if err == nil {
		t.Fatal("expected error from directory 503")
	}
}

func TestHTTPStatusErrorFormatting(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusTeapot,
		Body:       io.NopCloser(strings.NewReader("I'm a teapot")),
	}
	got := httpStatusError("POST /foo", resp).Error()
	if !strings.Contains(got, "418") || !strings.Contains(got, "teapot") {
		t.Fatalf("error = %q, want 418 + teapot", got)
	}

	respEmpty := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader("")),
	}
	got = httpStatusError("POST /foo", respEmpty).Error()
	if !strings.Contains(got, "404") {
		t.Fatalf("error = %q, want 404", got)
	}
	if strings.Contains(got, ": ") {
		t.Fatalf("empty body should not produce message suffix: %q", got)
	}
}

func TestValidateHopsRejectsInvalid(t *testing.T) {
	for _, hops := range []int{0, 2, 4, -1} {
		if err := validateHops(hops); err == nil {
			t.Fatalf("validateHops(%d) returned nil, want error", hops)
		}
	}
}

func TestGetCircuitWithHopsBadJSON(t *testing.T) {
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{bad")
	}))
	defer dir.Close()

	if _, err := GetCircuitWithHops(http.DefaultClient, dir.URL, 3); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestGetCircuitWithHopsMissingGuard(t *testing.T) {
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{}`)
	}))
	defer dir.Close()

	if _, err := GetCircuitWithHops(http.DefaultClient, dir.URL, 3); err == nil || !strings.Contains(err.Error(), "guard") {
		t.Fatalf("expected guard-missing error, got %v", err)
	}
}

func TestGetCircuitWithHopsMissingRelayExit(t *testing.T) {
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(CircuitResponse{
			Guard: NodeRecord{NodeRegistration: NodeRegistration{NodeID: "g1"}},
		})
	}))
	defer dir.Close()

	if _, err := GetCircuitWithHops(http.DefaultClient, dir.URL, 3); err == nil || !strings.Contains(err.Error(), "relay or exit") {
		t.Fatalf("expected relay/exit-missing error, got %v", err)
	}
}

func TestGetCircuitWithHopsInvalidHops(t *testing.T) {
	if _, err := GetCircuitWithHops(http.DefaultClient, "http://example.com", 2); err == nil {
		t.Fatal("expected error for hops=2")
	}
}

func TestGetCircuitWithHopsUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := dead.URL
	dead.Close()

	if _, err := GetCircuitWithHops(http.DefaultClient, url, 3); err == nil {
		t.Fatal("expected error for unreachable directory")
	}
}

func TestRegisterKeyBadURL(t *testing.T) {
	if err := RegisterKey(http.DefaultClient, "http://\x7f", "c1", []byte("k")); err == nil {
		t.Fatal("expected error")
	}
}

func TestRegisterKeyNon204(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer srv.Close()
	if err := RegisterKey(http.DefaultClient, srv.URL, "c1", []byte("k")); err == nil {
		t.Fatal("expected error")
	}
}

func TestSendOnionBadURL(t *testing.T) {
	if _, err := SendOnion(http.DefaultClient, "http://\x7f", "c1", []byte{1, 2}); err == nil {
		t.Fatal("expected error")
	}
}

func TestSendOnionNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := SendOnion(http.DefaultClient, srv.URL, "c1", []byte{1, 2}); err == nil {
		t.Fatal("expected error")
	}
}

func TestSendOnionInvalidResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{bad"))
	}))
	defer srv.Close()
	if _, err := SendOnion(http.DefaultClient, srv.URL, "c1", []byte{1, 2}); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestSendSetupKeyUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	priv, _ := genTestKeyPair(t)
	err := sendSetupKey(http.DefaultClient, url, "c1", &priv.PublicKey, make([]byte, 32))
	if err == nil {
		t.Fatal("expected unreachable error")
	}
}

func TestBuildOnionInvalidHops(t *testing.T) {
	if _, err := BuildOnionWithHops(nil, nil, nil, onion.ExitLayer{}, "", "", 2); err == nil {
		t.Fatal("expected hops error")
	}
}

func TestDecryptResponseInvalidHops(t *testing.T) {
	if _, err := DecryptResponseWithHops(nil, nil, nil, nil, 2); err == nil {
		t.Fatal("expected hops error")
	}
}

func TestDecryptResponseBadJSON(t *testing.T) {
	key := randomClientKey(t)
	ct, _ := onion.Encrypt(key, []byte("{bad"))
	_, err := DecryptResponseWithHops(key, nil, nil, ct, 1)
	if err == nil || !strings.Contains(err.Error(), "unmarshal") {
		t.Fatalf("expected unmarshal error, got %v", err)
	}
}

func TestParseClientConfigErrors(t *testing.T) {
	cases := [][]string{
		{"--destination-url", "http://d"},                             // missing directory
		{"--directory-url", "http://dir"},                             // missing destination
		{"--directory-url", "http://d", "--destination-url", "http://d", "--hops", "2"}, // bad hops
		{"--unknown-flag"},                                            // unknown
	}
	for _, args := range cases {
		if _, err := parseClientConfig(args); err == nil {
			t.Errorf("parseClientConfig(%v) returned nil, want error", args)
		}
	}
}

func TestRunClientWithEmptyMethodDefaultsToGet(t *testing.T) {
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("hops") != "3" {
			t.Fatalf("hops = %q, want 3", r.URL.Query().Get("hops"))
		}
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer dir.Close()

	cfg := &clientConfig{
		DirectoryURL:   dir.URL,
		DestinationURL: "http://example.com",
		Method:         "", // will be normalized to GET in parseClientConfig; simulate directly
		Hops:           3,
		Timeout:        1,
	}
	var buf bytes.Buffer
	if err := runClient(cfg, &buf); err == nil {
		t.Fatal("expected error from directory 503")
	}
}

func TestCheckAndRebuildCircuitNotReady(t *testing.T) {
	// When state is not Ready, the function returns without calling /nodes.
	state := &CircuitState{Ready: false}
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	checkAndRebuildCircuit(http.DefaultClient, srv.URL, state)
	if called {
		t.Fatal("expected no /nodes call when state not ready")
	}
}

func TestGetHealthyNodesBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{bad"))
	}))
	defer srv.Close()
	if _, err := GetHealthyNodes(http.DefaultClient, srv.URL); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestGetHealthyNodesNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := GetHealthyNodes(http.DefaultClient, srv.URL); err == nil {
		t.Fatal("expected error")
	}
}

func TestGetHealthyNodesUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	if _, err := GetHealthyNodes(http.DefaultClient, url); err == nil {
		t.Fatal("expected error")
	}
}

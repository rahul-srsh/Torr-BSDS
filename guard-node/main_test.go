package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rahul-srsh/Torr-BSDS/shared/config"
	"github.com/rahul-srsh/Torr-BSDS/shared/node"
	sharedserver "github.com/rahul-srsh/Torr-BSDS/shared/server"
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

func TestForwardEchoHandlerInvalidUpstreamURL(t *testing.T) {
	// A target URL containing a control character makes NewRequestWithContext fail.
	handler := forwardEchoHandler("http://\x7f", http.DefaultClient)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/forward/echo", nil)
	handler(recorder, req)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
}

func fakeGuardIdentity(t *testing.T) *node.Identity {
	t.Helper()
	priv, pub, err := node.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	return &node.Identity{NodeID: "test-node-id", PrivateKey: priv, PublicKeyPEM: pub, Host: "127.0.0.1"}
}

func overrideGuardHooks(t *testing.T, identity *node.Identity, idErr error) (called *bool, started **sharedserver.BaseServer) {
	t.Helper()
	origLoad := loadConfig
	origNew := newIdentity
	origStart := startServer
	origReg := startRegistration
	origFwd := getForwardTarget
	t.Cleanup(func() {
		loadConfig = origLoad
		newIdentity = origNew
		startServer = origStart
		startRegistration = origReg
		getForwardTarget = origFwd
	})

	loadConfig = func() *config.NodeConfig {
		return &config.NodeConfig{Port: "0", NodeType: "guard", DirectoryServerURL: "http://localhost:1"}
	}
	newIdentity = func() (*node.Identity, error) { return identity, idErr }

	var reg bool
	startRegistration = func(context.Context, *node.Config) { reg = true }

	var srv *sharedserver.BaseServer
	startServer = func(s *sharedserver.BaseServer) { srv = s }

	getForwardTarget = func() string { return "http://example.invalid" }

	return &reg, &srv
}

func TestRunWiresUpGuardRoutes(t *testing.T) {
	regCalled, started := overrideGuardHooks(t, fakeGuardIdentity(t), nil)

	if err := run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !*regCalled {
		t.Fatal("startRegistration was not called")
	}
	if *started == nil {
		t.Fatal("startServer was not called")
	}

	for _, path := range []string{"/health", "/key", "/onion", "/setup", "/forward/echo"} {
		recorder := httptest.NewRecorder()
		(*started).Mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code == http.StatusNotFound {
			t.Errorf("route %s not registered", path)
		}
	}
}

func TestRunPropagatesIdentityError(t *testing.T) {
	overrideGuardHooks(t, nil, errIdentityFailed)
	if err := run(); err != errIdentityFailed {
		t.Fatalf("run() err = %v, want %v", err, errIdentityFailed)
	}
}

func TestMainDelegatesToRun(t *testing.T) {
	overrideGuardHooks(t, fakeGuardIdentity(t), nil)
	main()
}

var errIdentityFailed = &identityFailedError{}

type identityFailedError struct{}

func (*identityFailedError) Error() string { return "identity failed" }

func TestDefaultHooksAreExecutable(t *testing.T) {
	id, err := defaultNewIdentity()
	if err != nil {
		t.Fatalf("defaultNewIdentity: %v", err)
	}
	if id.NodeID == "" || id.PrivateKey == nil {
		t.Fatal("default identity not populated")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	defaultStartRegistration(ctx, &node.Config{
		NodeID:            id.NodeID,
		NodeType:          "guard",
		DirectoryURL:      "http://127.0.0.1:1",
		HTTPClient:        &http.Client{Timeout: 10 * time.Millisecond},
		HeartbeatInterval: time.Hour,
	})

	srv := sharedserver.New(&config.NodeConfig{Port: freePort(t), NodeType: "guard-default"})
	go defaultStartServer(srv)
	waitForServer(t, "http://127.0.0.1:"+srv.Config.Port+"/health")
}

func TestDefaultGetForwardTargetReadsEnv(t *testing.T) {
	t.Setenv("FORWARD_TARGET_URL", "http://echo.local:8080/")
	if got := defaultGetForwardTarget(); got != "http://echo.local:8080" {
		t.Fatalf("defaultGetForwardTarget() = %q, want http://echo.local:8080", got)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := listener.Addr().(*net.TCPAddr)
	listener.Close()
	return strconv.Itoa(addr.Port)
}

func waitForServer(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s did not become healthy", url)
}

func TestGetForwardTargetTrimsTrailingSlash(t *testing.T) {
	t.Setenv("FORWARD_TARGET_URL", "http://echo.local:8080///")
	got := getForwardTarget()
	want := "http://echo.local:8080"
	if got != want {
		t.Fatalf("getForwardTarget() = %q, want %q", got, want)
	}
}

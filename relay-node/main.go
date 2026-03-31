package main

import (
	"net/http"
	"time"

	"github.com/rahul-srsh/Torr-BSDS/shared/config"
	onion "github.com/rahul-srsh/Torr-BSDS/shared/onion"
	sharedserver "github.com/rahul-srsh/Torr-BSDS/shared/server"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

func main() {
	cfg := config.Load()
	srv := sharedserver.New(cfg)

	keys := onion.NewKeyStore()
	h := onion.NewHandler(keys, httpClient, "relay")
	srv.Mux.HandleFunc("/key", h.HandleKey)
	srv.Mux.HandleFunc("/onion", h.HandleOnion)

	srv.Start()
}

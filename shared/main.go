package main

import (
	"github.com/rahul-srsh/Torr-BSDS/shared/config"
	"github.com/rahul-srsh/Torr-BSDS/shared/server"
)

func main() {
	cfg := config.Load()
	srv := server.New(cfg)
	srv.Start()
}
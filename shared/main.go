package main

import (
	"github.com/rahul-srsh/Torr-BSDS/shared/config"
	"github.com/rahul-srsh/Torr-BSDS/shared/server"
)

var (
	loadConfig  = config.Load
	startServer = defaultStartServer
)

func defaultStartServer(s *server.BaseServer) { s.Start() }

func run() {
	cfg := loadConfig()
	srv := server.New(cfg)
	startServer(srv)
}

func main() {
	run()
}

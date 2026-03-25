package config

import (
	"log"
	"os"
)

type NodeConfig struct {
	Port              string
	NodeType          string
	DirectoryServerURL string
}

func Load() *NodeConfig {
	cfg := &NodeConfig{
		Port:              getEnv("PORT", "8080"),
		NodeType:          getEnv("NODE_TYPE", "unknown"),
		DirectoryServerURL: getEnv("DIRECTORY_SERVER_URL", "http://localhost:8080"),
	}

	log.Printf("[config] PORT=%s", cfg.Port)
	log.Printf("[config] NODE_TYPE=%s", cfg.NodeType)
	log.Printf("[config] DIRECTORY_SERVER_URL=%s", cfg.DirectoryServerURL)

	return cfg
}

func getEnv(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return fallback
}
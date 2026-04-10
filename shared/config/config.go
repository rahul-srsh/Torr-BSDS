package config

import (
	"log"
	"os"
	"time"
)

type NodeConfig struct {
	Port                     string
	NodeType                 string
	DirectoryServerURL       string
	HeartbeatCleanupInterval time.Duration
	HeartbeatTimeout         time.Duration
}

func Load() *NodeConfig {
	cfg := &NodeConfig{
		Port:                     getEnv("PORT", "8080"),
		NodeType:                 getEnv("NODE_TYPE", "unknown"),
		DirectoryServerURL:       mustGetEnv("DIRECTORY_SERVER_URL"),
		HeartbeatCleanupInterval: getDurationEnv("HEARTBEAT_CLEANUP_INTERVAL", 15*time.Second),
		HeartbeatTimeout:         getDurationEnv("HEARTBEAT_TIMEOUT", 30*time.Second),
	}

	log.Printf("[config] PORT=%s", cfg.Port)
	log.Printf("[config] NODE_TYPE=%s", cfg.NodeType)
	log.Printf("[config] DIRECTORY_SERVER_URL=%s", cfg.DirectoryServerURL)
	log.Printf("[config] HEARTBEAT_CLEANUP_INTERVAL=%s", cfg.HeartbeatCleanupInterval)
	log.Printf("[config] HEARTBEAT_TIMEOUT=%s", cfg.HeartbeatTimeout)

	return cfg
}

func getEnv(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return fallback
}

func mustGetEnv(key string) string {
	val, ok := os.LookupEnv(key)
	if !ok || val == "" {
		log.Fatalf("[config] required environment variable %s is not set", key)
	}
	return val
}

func getDurationEnv(key string, fallback time.Duration) time.Duration {
	val, ok := os.LookupEnv(key)
	if !ok || val == "" {
		return fallback
	}

	parsed, err := time.ParseDuration(val)
	if err != nil {
		log.Printf("[config] invalid %s=%q; using default %s", key, val, fallback)
		return fallback
	}

	return parsed
}

package main

import (
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/hanzelhira/sandbox-executor/internal/api"
	"github.com/hanzelhira/sandbox-executor/internal/executor"
	"github.com/hanzelhira/sandbox-executor/web"
)

func main() {
	token := os.Getenv("SANDBOX_TOKEN")
	if token == "" {
		log.Fatal("SANDBOX_TOKEN must be set")
	}
	image := os.Getenv("SANDBOX_IMAGE")
	if image == "" {
		image = "python:3.12-slim"
	}
	addr := os.Getenv("SANDBOX_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	capacity := api.DefaultCapacity()
	if v := envInt("SANDBOX_MAX_CONCURRENT"); v > 0 {
		capacity.MaxConcurrent = v
	}
	if v := envInt("SANDBOX_MAX_QUEUED"); v >= 0 {
		capacity.MaxQueued = v
	}

	exec, err := executor.NewDockerExecutor(image)
	if err != nil {
		log.Fatalf("docker executor: %v", err)
	}
	srv := api.NewServerWithCapacity(exec, token, capacity)

	assets, err := web.Assets()
	if err != nil {
		log.Fatalf("embedded frontend: %v", err)
	}

	log.Printf("sandbox-executor listening on %s (image %s, max %d concurrent + %d queued)",
		addr, image, capacity.MaxConcurrent, capacity.MaxQueued)
	log.Printf("open http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, srv.HandlerWithUI(assets)))
}

// envInt returns the integer value of an env var, or -1 if unset/invalid.
func envInt(key string) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return -1
	}
	return v
}

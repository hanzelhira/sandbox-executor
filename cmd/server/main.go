package main

import (
	"log"
	"net/http"
	"os"

	"github.com/hanzelhira/sandbox-executor/internal/api"
	"github.com/hanzelhira/sandbox-executor/internal/executor"
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

	exec, err := executor.NewDockerExecutor(image)
	if err != nil {
		log.Fatalf("docker executor: %v", err)
	}
	srv := api.NewServer(exec, token)

	log.Printf("sandbox-executor listening on %s (image %s)", addr, image)
	log.Fatal(http.ListenAndServe(addr, srv.Handler()))
}

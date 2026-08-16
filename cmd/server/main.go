package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

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

	// Destroy any live sandboxes on Ctrl-C / SIGTERM so we never leave
	// orphaned containers behind.
	httpSrv := &http.Server{Addr: addr, Handler: srv.HandlerWithUI(assets)}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Println("shutting down, destroying live sessions...")
		srv.Shutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()

	log.Printf("sandbox-executor listening on %s (image %s, max %d concurrent + %d queued)",
		addr, image, capacity.MaxConcurrent, capacity.MaxQueued)
	log.Printf("open http://localhost%s", addr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// envInt returns the integer value of an env var, or -1 if unset/invalid.
func envInt(key string) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return -1
	}
	return v
}

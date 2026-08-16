BINARY := sandbox-executor

.PHONY: all build ui run dev test test-short clean

## build: frontend then binary — the UI must be built first, since
## //go:embed bakes web/dist into the executable at compile time.
all: build

ui:
	npm --prefix web ci --silent || npm --prefix web install --silent
	npm --prefix web run build
	@# vite empties dist/, which removes the tracked placeholder that keeps
	@# //go:embed compiling on a clone with no build output. Restore it.
	@touch web/dist/.gitkeep

build: ui
	go build -o $(BINARY) ./cmd/server

## run: single binary serving API + UI on :8080
run: build
	SANDBOX_TOKEN=$${SANDBOX_TOKEN:-devtoken} ./$(BINARY)

## dev: run BOTH servers together (API :8080 + hot-reloading UI :5173).
## Ctrl-C stops both; if either dies the other is torn down too.
dev:
	@./hack/dev.sh

## test: everything, including integration tests that need a Docker daemon
test:
	go test ./...

## test-short: skips anything requiring Docker
test-short:
	go test -short ./...

clean:
	rm -f $(BINARY)
	rm -rf web/dist/assets web/dist/index.html
	@touch web/dist/.gitkeep

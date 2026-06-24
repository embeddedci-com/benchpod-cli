BIN_DIR        := bin
CLI_BIN        := $(BIN_DIR)/benchpod

.PHONY: build run test vet fmt tidy clean

build:
	mkdir -p $(BIN_DIR)
	go build -o $(CLI_BIN) ./cmd/benchpod-cli

# Build and run the CLI (pass args via ARGS="...").
run: build
	@$(CLI_BIN) $(ARGS)

test:
	go test ./... -count=1

vet:
	go vet ./...

fmt:
	go fmt ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR)

BINARY     := mail-shadow-mcp
MODULE     := github.com/benja/mail-shadow-mcp
CMD        := ./cmd/mail-shadow-mcp
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS    := -ldflags "-s -w -X main.version=$(VERSION)"

# Output directory for cross-compiled binaries
DIST := dist

.PHONY: all build clean test lint install release

## build: Build for the current platform
build:
	go build $(LDFLAGS) -o $(BINARY)$(if $(filter windows,$(GOOS)),.exe,) $(CMD)

## install: Install to $GOPATH/bin
install:
	go install $(LDFLAGS) $(CMD)

## test: Run all tests
test:
	go test ./...

## lint: Run go vet
lint:
	go vet ./...

## clean: Remove build artifacts
clean:
	rm -rf $(DIST) $(BINARY) $(BINARY).exe

## release: Cross-compile for all platforms into dist/
release: clean
	@mkdir -p $(DIST)

	@echo "Building Windows amd64..."
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(DIST)/$(BINARY)-windows-amd64.exe $(CMD)

	@echo "Building Windows arm64..."
	GOOS=windows GOARCH=arm64 go build $(LDFLAGS) -o $(DIST)/$(BINARY)-windows-arm64.exe $(CMD)

	@echo "Building Linux amd64..."
	GOOS=linux   GOARCH=amd64 go build $(LDFLAGS) -o $(DIST)/$(BINARY)-linux-amd64 $(CMD)

	@echo "Building Linux arm64..."
	GOOS=linux   GOARCH=arm64 go build $(LDFLAGS) -o $(DIST)/$(BINARY)-linux-arm64 $(CMD)

	@echo "Building macOS amd64..."
	GOOS=darwin  GOARCH=amd64 go build $(LDFLAGS) -o $(DIST)/$(BINARY)-darwin-amd64 $(CMD)

	@echo "Building macOS arm64 (Apple Silicon)..."
	GOOS=darwin  GOARCH=arm64 go build $(LDFLAGS) -o $(DIST)/$(BINARY)-darwin-arm64 $(CMD)

	@echo ""
	@echo "Binaries in $(DIST)/:"
	@ls -lh $(DIST)/

## version: Print current version
version:
	@echo $(VERSION)

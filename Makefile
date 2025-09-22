BINARY_NAME := chatlog
GO := go

# Embed version into binary
ifeq ($(VERSION),)
	VERSION := $(shell git describe --tags --always --dirty="-dev" 2>/dev/null)
endif
ifeq ($(VERSION),)
	VERSION := dev
endif
LDFLAGS := -ldflags '-X "github.com/sjzar/chatlog/pkg/version.Version=$(VERSION)" -w -s'

PLATFORMS := \
	darwin/amd64 \
	darwin/arm64 \
	linux/amd64 \
	linux/arm64 \
	windows/amd64 \
	windows/arm64

UPX_PLATFORMS := \
	darwin/amd64 \
	linux/amd64 \
	linux/arm64 \
	windows/amd64

.PHONY: all clean lint tidy test build upx

all: clean lint tidy test build

clean:
	@echo "🧹 Cleaning..."
	@rm -rf bin/

lint:
	@echo "🕵️‍♂️ Running linters..."
	golangci-lint run ./...

tidy:
	@echo "🧼 Tidying up dependencies..."
	$(GO) mod tidy

test:
	@echo "🧪 Running tests..."
	$(GO) test ./... -cover

build:
	@echo "🔨 Building for current platform..."
	@mkdir -p bin
	CGO_ENABLED=1 $(GO) build -trimpath $(LDFLAGS) -o bin/$(BINARY_NAME) main.go

build-windows:
	@echo "🪟 Cross-compiling for Windows amd64..."
	@mkdir -p bin
	@if [ "$(OS)" = "Windows_NT" ]; then \
		# On Windows host, don't force CC; rely on local toolchain (e.g. MSYS2 mingw64). \
		echo "🧭 Host detected: Windows"; \
		GOOS=windows GOARCH=amd64 CGO_ENABLED=1 $(GO) build -trimpath $(LDFLAGS) -o bin/$(BINARY_NAME)_windows_amd64.exe main.go; \
	else \
		# Non-Windows host: require mingw-w64 cross compiler. Use fallback if CC_WIN_AMD64 is empty. \
		ccbin=$${CC_WIN_AMD64:-x86_64-w64-mingw32-gcc}; \
		echo "🛠  Resolving MinGW: $$ccbin"; \
		if ! command -v "$$ccbin" >/dev/null 2>&1; then \
			echo "❌ $$ccbin not found in PATH."; \
			echo "   Arch Linux: sudo pacman -S --needed mingw-w64-gcc"; \
			exit 1; \
		fi; \
		echo "⚙️ Using CC=$$ccbin"; \
		env CC="$$ccbin" GOOS=windows GOARCH=amd64 CGO_ENABLED=1 $(GO) build -trimpath $(LDFLAGS) -o bin/$(BINARY_NAME)_windows_amd64.exe main.go; \
	fi
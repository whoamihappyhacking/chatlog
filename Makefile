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

# Cross-compiler hints (override if paths differ)
CC_WIN_AMD64 ?= x86_64-w64-mingw32-gcc
CC_WIN_ARM64 ?= aarch64-w64-mingw32-gcc
CC_DARWIN_AMD64 ?= o64-clang
CC_DARWIN_ARM64 ?= oa64-clang

.PHONY: all clean lint tidy test build upx

all: clean lint tidy test build

clean:
	@echo "🧹  Cleaning..."
	@rm -rf bin/

lint:
	@echo "🕵️‍♂️  Running linters..."
	golangci-lint run ./...

tidy:
	@echo "🧼  Tidying up dependencies..."
	$(GO) mod tidy

test:
	@echo "🧪  Running tests..."
	$(GO) test ./... -cover

build:
	@echo "🔨  Building for current platform..."
	@mkdir -p bin
	CGO_ENABLED=1 $(GO) build -trimpath $(LDFLAGS) -o bin/$(BINARY_NAME) main.go

buildcross:
	@echo "[cross] Interactive cross-compilation selection (Windows/macOS)"
	@mkdir -p bin
	@printf "\nSelect target platforms to build (multiple choices, space separated):\n  1) windows/amd64\n  2) windows/arm64\n  3) darwin/amd64 (macOS Intel)\n  4) darwin/arm64 (Apple Silicon)\n> "; \
	read choices; \
	for ch in $$choices; do \
		case $$ch in \
			1) \
				ccbin=$${CC_WIN_AMD64:-x86_64-w64-mingw32-gcc}; \
				: # On Windows host: do not force CC, use local toolchain \
				if [ "$(OS)" = "Windows_NT" ]; then \
					echo "🧭 Host: Windows"; \
					GOOS=windows GOARCH=amd64 CGO_ENABLED=1 $(GO) build -trimpath $(LDFLAGS) -o bin/$(BINARY_NAME)_windows_amd64.exe main.go; \
				else \
					if command -v "$$ccbin" >/dev/null 2>&1; then \
						echo "⚙️ Using CC=$$ccbin"; \
						env CC="$$ccbin" GOOS=windows GOARCH=amd64 CGO_ENABLED=1 $(GO) build -trimpath $(LDFLAGS) -o bin/$(BINARY_NAME)_windows_amd64.exe main.go; \
					else \
						echo "⏭️  Skip windows/amd64: $$ccbin not found (Arch: sudo pacman -S --needed mingw-w64-gcc)"; \
					fi; \
				fi \
			;; \
			2) \
				ccbin=$${CC_WIN_ARM64:-aarch64-w64-mingw32-gcc}; \
				: # windows/arm64 needs aarch64 toolchain \
				if [ "$(OS)" = "Windows_NT" ]; then \
					if command -v "$$ccbin" >/dev/null 2>&1; then \
						echo "⚙️  Using CC=$$ccbin"; \
						env CC="$$ccbin" GOOS=windows GOARCH=arm64 CGO_ENABLED=1 $(GO) build -trimpath $(LDFLAGS) -o bin/$(BINARY_NAME)_windows_arm64.exe main.go; \
					else \
						echo "⏭️  Skip windows/arm64: $$ccbin not found (MSYS2: pacman -S mingw-w64-aarch64-gcc)"; \
					fi; \
				else \
					if command -v "$$ccbin" >/dev/null 2>&1; then \
						echo "⚙️  Using CC=$$ccbin"; \
						env CC="$$ccbin" GOOS=windows GOARCH=arm64 CGO_ENABLED=1 $(GO) build -trimpath $(LDFLAGS) -o bin/$(BINARY_NAME)_windows_arm64.exe main.go; \
					else \
						echo "⏭️  Skip windows/arm64: $$ccbin not found (Arch: sudo pacman -S --needed mingw-w64-gcc)"; \
					fi; \
				fi \
			;; \
			3) \
				ccdarwin=$${CC_DARWIN_AMD64:-o64-clang}; \
				if [ "$(OS)" = "Windows_NT" ] || [ "$$(uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]')" = "linux" ]; then \
					: # Non-macOS hosts need osxcross o64-clang \
					if command -v "$$ccdarwin" >/dev/null 2>&1; then \
						echo "⚙️  Using CC=$$ccdarwin"; \
						env CC="$$ccdarwin" GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 $(GO) build -trimpath $(LDFLAGS) -o bin/$(BINARY_NAME)_darwin_amd64; \
					else \
						echo "⏭️  Skip darwin/amd64: $$ccdarwin not found (need osxcross)"; \
					fi; \
				else \
					: # macOS host: try system clang \
					if command -v clang >/dev/null 2>&1; then \
						echo "⚙️ Using CC=clang"; \
						env CC=clang GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 $(GO) build -trimpath $(LDFLAGS) -o bin/$(BINARY_NAME)_darwin_amd64; \
					else \
						echo "⏭️  Skip darwin/amd64: clang not found"; \
					fi; \
				fi \
			;; \
			4) \
				ccdarwin=$${CC_DARWIN_ARM64:-oa64-clang}; \
				if [ "$(OS)" = "Windows_NT" ] || [ "$$(uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]')" = "linux" ]; then \
					: # Non-macOS hosts need osxcross oa64-clang \
					if command -v "$$ccdarwin" >/dev/null 2>&1; then \
						echo "⚙️  Using CC=$$ccdarwin"; \
						env CC="$$ccdarwin" GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 $(GO) build -trimpath $(LDFLAGS) -o bin/$(BINARY_NAME)_darwin_arm64; \
					else \
						echo "⏭️  Skip darwin/arm64: $$ccdarwin not found (need osxcross)"; \
					fi; \
				else \
					: # macOS host: try system clang \
					if command -v clang >/dev/null 2>&1; then \
						echo "⚙️  Using CC=clang"; \
						env CC=clang GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 $(GO) build -trimpath $(LDFLAGS) -o bin/$(BINARY_NAME)_darwin_arm64; \
					else \
						echo "⏭️  Skip darwin/arm64: clang not found"; \
					fi; \
				fi \
			;; \
			*) \
				echo "❓ Unknown option: $$ch (valid: 1 2 3 4)"; \
			;; \
		esac; \
	done
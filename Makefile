BINARY_NAME := chatlog
GO := go
ifeq ($(VERSION),)
    VERSION := $(shell git describe --tags --always --dirty="-dev")
endif
LDFLAGS := -ldflags '-X "github.com/sjzar/chatlog/pkg/version.Version=$(VERSION)" -w -s'
TAG := --tags "fts5"
CGO_EXTRA_LDFLAGS :=
ifeq ($(OS),Windows_NT)
	CGO_EXTRA_LDFLAGS := -lgomp
endif
CGO_FLAG := CGO_ENABLED=1 CGO_CFLAGS="-I$(abspath $(CURDIR)/third_party/include)" CGO_LDFLAGS="-L$(abspath $(CURDIR)/third_party/lib) $(CGO_EXTRA_LDFLAGS)"

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

ifeq ($(OS),Windows_NT)
	MKDIR_BIN := powershell -NoProfile -Command "New-Item -ItemType Directory -Force -Path 'bin' | Out-Null"
	CLEAN_BIN := powershell -NoProfile -Command "if (Test-Path 'bin') { Remove-Item -Recurse -Force 'bin' }"
	BINARY_SUFFIX := .exe
else
	MKDIR_BIN := mkdir -p bin
	CLEAN_BIN := rm -rf bin
	BINARY_SUFFIX :=
endif

.PHONY: all clean lint tidy test build crossbuild upx

all: clean lint tidy test build

clean:
	@echo "Cleaning..."
	@$(CLEAN_BIN)

lint:
	@echo "Running linters..."
	golangci-lint run ./...

tidy:
	@echo "Tidying up dependencies..."
	@$(CGO_FLAG) $(GO) mod tidy

test:
	@echo "Running tests..."
	@$(CGO_FLAG) $(GO) test ./... -cover

build:
	@echo "Building for current platform..."
	@$(MKDIR_BIN)
	@$(CGO_FLAG) $(GO) build -trimpath $(LDFLAGS) $(TAG) -o bin/$(BINARY_NAME)$(BINARY_SUFFIX) main.go
	@echo "Done!"

crossbuild: clean
	@echo "Building for multiple platforms..."
	@$(MKDIR_BIN)
	for platform in $(PLATFORMS); do \
		os=$$(echo $$platform | cut -d/ -f1); \
		arch=$$(echo $$platform | cut -d/ -f2); \
		float=$$(echo $$platform | cut -d/ -f3); \
		output_name=bin/chatlog_$${os}_$${arch}; \
		[ "$$float" != "" ] && output_name=$$output_name_$$float; \
		[ "$$os" = "windows" ] && output_name=$$output_name.exe; \
		echo "Building for $$os/$$arch..."; \
		echo "Building for $$output_name..."; \
		@GOOS=$$os GOARCH=$$arch GOARM=$$float $(CGO_FLAG) $(GO) build -trimpath $(LDFLAGS) $(TAG) -o $$output_name main.go ; \
		if [ "$(ENABLE_UPX)" = "1" ] && echo "$(UPX_PLATFORMS)" | grep -q "$$os/$$arch"; then \
			echo "Compressing binary $$output_name..." && upx --best $$output_name; \
		fi; \
	done
	@echo "Done!"
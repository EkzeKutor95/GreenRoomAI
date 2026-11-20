SHELL := /bin/sh
# Keep generated dependencies, build caches and temporary files inside this project.
export GOCACHE := $(CURDIR)/.cache/go-build
export GOMODCACHE := $(CURDIR)/.cache/go-mod
export GOPATH := $(CURDIR)/.cache/go
export GOTMPDIR := $(CURDIR)/.cache/tmp
export TMPDIR := $(CURDIR)/.cache/tmp
export GOTOOLCHAIN := local

.PHONY: prepare run build test check fmt vet windows demo check-config check-api diagnose-iiko check-workforce
prepare:
	@mkdir -p .cache/tmp bin
run: prepare
	go run ./cmd/greenroom
build: prepare
	go build -trimpath -o bin/greenroom-ai ./cmd/greenroom
windows: prepare
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o bin/greenroom-ai-windows-amd64.exe ./cmd/greenroom
test: prepare
	go test ./...
fmt: prepare
	go fmt ./...
vet: prepare
	go vet ./...
check: fmt vet test
	go build ./...
check-config: prepare
	go run ./cmd/greenroom --check-config
check-api: prepare
	go run ./cmd/greenroom --check
demo: prepare
	go run ./cmd/greenroom --demo

diagnose-iiko: prepare
	go run ./cmd/greenroom --diagnose-iiko $(if $(DATE),--date $(DATE),)

check-workforce: prepare
	go run ./cmd/greenroom --check-workforce $(if $(DATE),--date $(DATE),)

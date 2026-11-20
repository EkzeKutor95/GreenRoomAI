.DEFAULT_GOAL := help

# Keep dependencies, caches and temporary files inside this project on every OS.
export GOCACHE := $(CURDIR)/.cache/go-build
export GOMODCACHE := $(CURDIR)/.cache/go-mod
export GOPATH := $(CURDIR)/.cache/go
export GOTMPDIR := $(CURDIR)/.cache/tmp
export TMPDIR := $(CURDIR)/.cache/tmp
export TMP := $(CURDIR)/.cache/tmp
export TEMP := $(CURDIR)/.cache/tmp
export GOTOOLCHAIN := local

RESTART_INTERVAL ?= 1h

ifeq ($(OS),Windows_NT)
SHELL := powershell.exe
.SHELLFLAGS := -NoLogo -NoProfile -NonInteractive -Command
EXE := .exe
HELP_NOOP := exit 0
NATIVE_PREFIX := $$ErrorActionPreference = 'Stop';
NATIVE_EXIT := ; exit $$LASTEXITCODE
RUN_BINARY = & './$(BINARY)'
PREPARE_DIRS = New-Item -ItemType Directory -Force -ErrorAction Stop -Path '.cache/tmp', 'bin' | Out-Null
INIT_ENV = $$ErrorActionPreference = 'Stop'; if (Test-Path -LiteralPath '.env') { Write-Output '.env already exists; kept unchanged.' } else { $$greenroomDir = (Get-Location).ProviderPath; [System.IO.File]::Copy((Join-Path $$greenroomDir '.env.example'), (Join-Path $$greenroomDir '.env'), $$false); Write-Output 'Created .env. Fill in its values before running.' }
else
SHELL := /bin/sh
.SHELLFLAGS := -ec
EXE :=
HELP_NOOP := :
NATIVE_PREFIX :=
NATIVE_EXIT :=
RUN_BINARY = './$(BINARY)'
PREPARE_DIRS = mkdir -p '.cache/tmp' 'bin'
INIT_ENV = if test -e .env || test -L .env; then printf '%s\n' '.env already exists; kept unchanged.'; else (umask 077; set -C; cat .env.example > .env) && printf '%s\n' 'Created .env. Fill in its values before running.'; fi
endif

BINARY := bin/greenroom-ai$(EXE)

.PHONY: help init prepare run run-once build test check fmt vet windows demo check-config check-api diagnose-iiko check-workforce

help:
	$(info GREEN ROOM AI - run these commands from the project directory)
	$(info )
	$(info make init            Create .env only if absent; never overwrite it)
	$(info make check-config    Validate local configuration without network access)
	$(info make run             Build and run; restart the worker every 1h)
	$(info make run-once        Run continuously without hourly restarts)
	$(info make run RESTART_INTERVAL=2h   Change the elapsed restart interval)
	$(info make build           Build a native executable in bin/)
	$(info make windows         Build bin/greenroom-ai-windows-amd64.exe)
	$(info make test            Run all tests without live service credentials)
	$(info make check           Run fmt, vet, tests and build)
	$(info make demo            Show a synthetic report without secrets or network)
	$(info make check-api       Read-only iiko and Telegram connection checks)
	$(info make diagnose-iiko [DATE=YYYY-MM-DD]   Diagnose a completed OLAP day)
	$(info make check-workforce [DATE=YYYY-MM-DD] Check OLAP cashiers and cash shifts)
	$(info Ctrl+C stops the application; it does not start another worker.)
	@$(HELP_NOOP)

prepare:
	@$(PREPARE_DIRS)

init:
	@$(INIT_ENV)

run: build
	$(NATIVE_PREFIX)$(RUN_BINARY) --restart-every="$(RESTART_INTERVAL)"$(NATIVE_EXIT)

run-once: build
	$(NATIVE_PREFIX)$(RUN_BINARY) --restart-every=0$(NATIVE_EXIT)

build: prepare
	$(NATIVE_PREFIX)go build -trimpath -o "$(BINARY)" ./cmd/greenroom$(NATIVE_EXIT)

windows: export GOOS := windows
windows: export GOARCH := amd64
windows: export CGO_ENABLED := 0
windows: prepare
	$(NATIVE_PREFIX)go build -trimpath -o bin/greenroom-ai-windows-amd64.exe ./cmd/greenroom$(NATIVE_EXIT)

test: prepare
	$(NATIVE_PREFIX)go test ./...$(NATIVE_EXIT)

fmt: prepare
	$(NATIVE_PREFIX)go fmt ./...$(NATIVE_EXIT)

vet: prepare
	$(NATIVE_PREFIX)go vet ./...$(NATIVE_EXIT)

check: fmt vet test
	$(NATIVE_PREFIX)go build ./...$(NATIVE_EXIT)

check-config: build
	$(NATIVE_PREFIX)$(RUN_BINARY) --check-config$(NATIVE_EXIT)

check-api: build
	$(NATIVE_PREFIX)$(RUN_BINARY) --check$(NATIVE_EXIT)

demo: build
	$(NATIVE_PREFIX)$(RUN_BINARY) --demo$(NATIVE_EXIT)

diagnose-iiko: build
	$(NATIVE_PREFIX)$(RUN_BINARY) --diagnose-iiko $(if $(DATE),--date "$(DATE)",)$(NATIVE_EXIT)

check-workforce: build
	$(NATIVE_PREFIX)$(RUN_BINARY) --check-workforce $(if $(DATE),--date "$(DATE)",)$(NATIVE_EXIT)

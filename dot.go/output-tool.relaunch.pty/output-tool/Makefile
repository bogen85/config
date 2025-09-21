# CudaText: lexer_file=Makefile; tab_size=2; tab_spaces=No;

MAKEFLAGS := \
	--warn-undefined-variables \
	--no-builtin-rules \
	--no-builtin-variables \
	--output-sync=target \
	--silent

.ONESHELL:
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

export SHELLOPTS

define build_defs
	function : () {
		if [ "@" == "$$1" ]; then
			shift
		else
			echo '++' "$$@"
		fi
		"$$@"
	}
endef

_ = $(build_defs); printf '[%s]\n' '$@';

APP := output-tool
ROOT := $(CURDIR)
SRC  := $(ROOT)/src
BIN  := $(ROOT)/bin

GOOS         ?= linux
GOARCH       ?= amd64
CGO_ENABLED  := 0
GO111MODULE  := off
GOPATH       := /usr/share/gocode:$(ROOT)
export GOOS GOARCH CGO_ENABLED GO111MODULE GOPATH

GO := go

.PHONY: all build fmt clean run-file run-pipe tar default

default:
	$_
	echo $$GOPATH

all: build
	$_

build:
	$_
	: @ mkdir -p $(BIN)
	: @ $(GO) build -o $(BIN)/$(APP) -v $(SRC)/*.go

install: build
	$_
	: cp -vf $(BIN)/$(APP) $(HOME)/bin/ot

fmt:
	$_
	for go in $$(find $(SRC) -name '*.go'); do : @ $(GO) fmt $${go}; done

clean:
	$_
	: rm -rf $(BIN) *.tar.gz

run-file: build
	$_
	$(BIN)/$(APP) --file=./README.md

run-pipe: build
	$_
	: echo "example: pipe mode"; echo "main.go:10:2: oops" | $(BIN)/$(APP) --pipe

tar:
	$_
	: tar -czf $(APP)-src.tar.gz --exclude='./bin' -C $(ROOT) .
	: echo "Created $(APP)-src.tar.gz"

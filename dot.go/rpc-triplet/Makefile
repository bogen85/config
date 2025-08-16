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

_ = printf '[%s]\n' '$@';

GOOS         := linux
GOARCH       := amd64
CGO_ENABLED  := 0
GO111MODULE  := auto
GOPATH       := /usr/share/gocode
export GOOS GOARCH CGO_ENABLED GO111MODULE GOPATH

SRC  := src
BIN  := bin

# PROGS := broker server client
PROGS := $(basename $(notdir $(wildcard $(SRC)/*.go)))

BINS := $(addprefix $(BIN)/,$(PROGS))

.PHONY: default all build clean fmt

default:
	$_
	echo SHELLOPTS=$$SHELLOPTS

all:
	$_
	time make build -j

build: $(BINS)
	$_

clean:
	$_
	rm -rvf $(BIN)/*

fmt:
	$_
	find . -type f -name "*.go" -exec go fmt {} \;

$(BIN):
	$_
	mkdir -pv $@

$(BIN)/%: $(SRC)/%.go ${MAKEFILE_LIST} | $(BIN)
	$_
	set -x
	go build -a -o $@ $<

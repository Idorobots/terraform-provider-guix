TOFU ?= tofu
GUIX ?= guix

default: fmt lint install generate

build:
	go build -v ./...

install: build
	go install -v ./...

lint:
	golangci-lint run

generate:
	TOFU="$(TOFU)" go -C tools generate ./...

fmt:
	gofmt -s -w -e .

test:
	go test -v -cover -timeout=120s -parallel=10 ./...

testacc:
	TOFU="$(TOFU)" TF_ACC=1 go test -v -cover -timeout 120m ./...

test-channel:
	$(GUIX) repl -L channel/modules channel/tests/secrets.scm

.PHONY: fmt lint test testacc build install generate test-channel

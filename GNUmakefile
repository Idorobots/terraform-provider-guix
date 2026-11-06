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
	GUIX_SECRETS_BUILD_HELPER=1 $(GUIX) repl -L channel/modules channel/tests/secrets.scm

test-channel-images:
	PYTHONDONTWRITEBYTECODE=1 $(GUIX) shell e2fsprogs util-linux -- python3 -m unittest discover -s channel/tests -p 'test_*.py' -v

.PHONY: fmt lint test testacc build install generate test-channel test-channel-images

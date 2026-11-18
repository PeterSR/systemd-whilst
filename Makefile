VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet check install uninstall clean

build:
	go build -ldflags "-X main.version=$(VERSION)" -o whilst ./cmd/whilst

test:
	go test ./...

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

check: vet test

# The binary installs itself: see `whilst install`.
install: build
	./whilst install

uninstall: build
	./whilst uninstall
	rm -f $(HOME)/.local/bin/whilst

clean:
	rm -f whilst

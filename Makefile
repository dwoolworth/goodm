VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo "dev")
LDFLAGS  = -ldflags "-X main.version=$(VERSION)"

.PHONY: build test test-race lint clean

build:
	go build $(LDFLAGS) -o bin/goodm ./cmd/goodm

test:
	go test -race -count=1 ./...

test-short:
	go test -race -short -count=1 ./...

lint:
	go vet ./...

clean:
	rm -rf bin/

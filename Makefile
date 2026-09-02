BINARY  := opamela
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO      ?= go
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt cover fuzz corpus docker clean

all: build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/opamela

vet:
	$(GO) vet ./...
	@test -z "$$(gofmt -l . )" || { echo "not gofmt-clean:"; gofmt -l .; exit 1; }

test: vet
	$(GO) test -race ./...

cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

# The scanner is hand written; fuzz it.
fuzz:
	$(GO) test ./internal/opamfile/ -run XXX -fuzz FuzzParse -fuzztime 60s
	$(GO) test ./internal/mirror/ -run XXX -fuzz FuzzArchiveName -fuzztime 30s

# Run the parser and the builder over a real opam-repository checkout.
# Usage: make corpus CORPUS=/path/to/opam-repository
CORPUS ?=
corpus:
	@test -n "$(CORPUS)" || { echo "set CORPUS=/path/to/opam-repository"; exit 2; }
	OPAMELA_CORPUS=$(CORPUS) $(GO) test ./... -run Corpus -v -timeout 20m

docker:
	docker build --build-arg VERSION=$(VERSION) -t opamela:$(VERSION) .

clean:
	rm -f $(BINARY) coverage.out coverage.html

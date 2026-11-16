GOLANGCI_LINT ?= golangci-lint

.PHONY: build test lint fmt fmt-check ci install

build:
	go build -o bin/prf ./cmd/prf

install:
	go install ./cmd/prf

test:
	go test -race ./...

lint:
	$(GOLANGCI_LINT) run ./...

fmt:
	$(GOLANGCI_LINT) fmt ./...

fmt-check:
	@out=$$($(GOLANGCI_LINT) fmt --diff ./...); if [ -n "$$out" ]; then echo "$$out"; echo "run: make fmt"; exit 1; fi

# ci is exactly what GitHub Actions runs; run it before pushing.
ci: fmt-check lint test build

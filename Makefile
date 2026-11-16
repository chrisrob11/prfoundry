GOLANGCI_LINT ?= golangci-lint
# COVER_FLOOR is the lowest total statement coverage `make cover-check` (and so `make ci`)
# accepts, in percent, measured across the whole module (-coverpkg), so code the end-to-end
# tests reach counts. It sits about a point under the total because linux and macos differ
# slightly. Raise it when coverage rises; never lower it to pass.
COVER_FLOOR ?= 89

.PHONY: build test cover cover-check lint fmt fmt-check ci install

build:
	go build -o bin/prf ./cmd/prf

install:
	go install ./cmd/prf

test:
	go test -race -count=1 ./...

# cover runs the tests once, writing coverage.out, and prints the total.
cover:
	go test -race -count=1 -coverpkg=./... -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -1

cover-check: cover
	@total=$$(go tool cover -func=coverage.out | tail -1 | awk '{sub(/%/, "", $$NF); print $$NF}'); \
	if awk -v t="$$total" -v f="$(COVER_FLOOR)" 'BEGIN { exit !(t < f) }'; then \
		echo "coverage $$total% is below the floor of $(COVER_FLOOR)%"; exit 1; \
	else \
		echo "coverage $$total% meets the floor of $(COVER_FLOOR)%"; \
	fi

lint:
	$(GOLANGCI_LINT) run ./...

fmt:
	$(GOLANGCI_LINT) fmt ./...

fmt-check:
	@out=$$($(GOLANGCI_LINT) fmt --diff ./...); if [ -n "$$out" ]; then echo "$$out"; echo "run: make fmt"; exit 1; fi

# ci is exactly what GitHub Actions runs; run it before pushing.
# cover-check runs the tests, so ci does not run them a second time.
ci: fmt-check lint cover-check build

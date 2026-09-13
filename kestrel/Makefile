GO      ?= go
BIN     ?= build
LDFLAGS ?= -s -w
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64

.PHONY: all build test race vet fmt lint bench fuzz cover clean run cli release check

all: check build

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/kestreld ./cmd/kestreld
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/kestrel-cli ./cmd/kestrel-cli

# check is what CI runs on every merge to main.
check: fmt vet race

fmt:
	@out=$$(gofmt -l . 2>/dev/null); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test -count=1 ./...

# NFR-5: the whole suite must be race clean. Non-negotiable.
race:
	$(GO) test -race -count=1 ./...

bench:
	$(GO) test ./... -run XXX -bench . -benchmem

cover:
	$(GO) test -coverprofile=$(BIN)/cover.out ./...
	$(GO) tool cover -func=$(BIN)/cover.out | tail -1

# The RESP parser is one of the places untrusted bytes enter the process, so
# it gets dedicated fuzzing time. M0's exit criterion is one clean hour.
fuzz:
	$(GO) test ./resp/ -run XXX -fuzz FuzzReadCommand -fuzztime $(or $(FUZZTIME),60s)

fuzz-hour:
	$(GO) test ./resp/ -run XXX -fuzz FuzzReadCommand -fuzztime 1h

release:
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  echo "building $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -ldflags '$(LDFLAGS)' \
	    -o $(BIN)/kestreld-$$os-$$arch ./cmd/kestreld || exit 1; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -ldflags '$(LDFLAGS)' \
	    -o $(BIN)/kestrel-cli-$$os-$$arch ./cmd/kestrel-cli || exit 1; \
	done

run: build
	$(BIN)/kestreld ./kestrel.conf --appendonly no --dir ./$(BIN)

cli: build
	$(BIN)/kestrel-cli

clean:
	rm -rf $(BIN)

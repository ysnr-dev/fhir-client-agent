BINARY  := fhir-client-agent
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# 院内機に Go も C ランタイムも要らない静的バイナリにする。
GOFLAGS := CGO_ENABLED=0
LDFLAGS := -trimpath -ldflags="-s -w -X main.version=$(VERSION)"

.PHONY: build dist linux-amd64 linux-arm64 windows-amd64 darwin test lint fmt clean

build:
	go build ./...

# 配布物一式。古いバイナリが混ざらないよう clean から。
dist: clean linux-amd64 linux-arm64 windows-amd64 darwin
	cp .env.example dist/.env.example
	cp deploy/fhir-client-agent.service dist/
	@ls -lh dist

linux-amd64:
	GOOS=linux GOARCH=amd64 $(GOFLAGS) go build $(LDFLAGS) -o dist/$(BINARY)-linux-amd64 .

linux-arm64:
	GOOS=linux GOARCH=arm64 $(GOFLAGS) go build $(LDFLAGS) -o dist/$(BINARY)-linux-arm64 .

windows-amd64:
	GOOS=windows GOARCH=amd64 $(GOFLAGS) go build $(LDFLAGS) -o dist/$(BINARY)-windows-amd64.exe .

darwin:
	GOOS=darwin GOARCH=arm64 $(GOFLAGS) go build $(LDFLAGS) -o dist/$(BINARY)-darwin-arm64 .

test:
	go test -race ./...

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

fmt:
	gofmt -w .

clean:
	rm -rf dist

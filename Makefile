.PHONY: run build test vet lint fmt clean docker docker-up docker-down help

# Default target
help:
	@echo "instant-lite-api — make targets:"
	@echo "  run          start the server (reads config.yaml)"
	@echo "  build        compile binary to bin/instant-lite"
	@echo "  test         run the Go test suite"
	@echo "  vet          go vet + gofmt check"
	@echo "  fmt          format sources in place"
	@echo "  lint         strict checks (vet + gofmt --diff)"
	@echo "  docker       build the Docker image"
	@echo "  docker-up    docker compose up -d --build"
	@echo "  docker-down  docker compose down -v"
	@echo "  clean        remove build artefacts"

run:
	go run ./cmd/server

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/instant-lite ./cmd/server

test:
	go test ./...

vet:
	go vet ./...
	@gofmt -l . | (! grep .) || (echo "gofmt wants changes — run 'make fmt'" && exit 1)

fmt:
	gofmt -w .

lint: vet
	@gofmt -d . | (! grep .) || (echo "gofmt diff above — run 'make fmt'" && exit 1)

docker:
	docker build -t instant-lite-api:local .

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down -v

clean:
	rm -rf bin/ lite instant-lite-api

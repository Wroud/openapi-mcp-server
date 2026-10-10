.PHONY: test lint build build-local build-lambda run dev smoke schema-check

test:
	go test -race ./...

lint:
	go vet ./...

build: build-local build-lambda

build-lambda:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -trimpath -ldflags='-s -w' -o dist/bootstrap ./cmd/openapi-mcp-lambda
	cd dist && zip -1 -q bootstrap.zip bootstrap

build-local:
	mkdir -p dist
	go build -trimpath -o dist/openapi-mcp-server ./cmd/openapi-mcp-server

run:
	go run ./cmd/openapi-mcp-server

dev:
	./scripts/dev.sh run

smoke:
	./scripts/dev.sh smoke

schema-check:
	./scripts/dev.sh schema

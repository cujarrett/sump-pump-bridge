.PHONY: ci lint test build

ci: lint test

lint:
	go mod tidy -diff
	golangci-lint run

test:
	go test ./...

build:
	go build -o sump-pump-consumer .

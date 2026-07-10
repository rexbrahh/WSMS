.PHONY: test vet race tidy build demo

test:
	go test ./...

vet:
	go vet ./...

race:
	go test -race ./...

tidy:
	go mod tidy

build:
	go build -o bin/wsms ./cmd/wsms

demo:
	go run ./cmd/wsms demo

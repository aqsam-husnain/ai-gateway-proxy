.PHONY: build run run-dev test clean tidy

build:
	go build -o bin/ai-proxy ./main.go

run:
	go run main.go

run-dev:
	go run main.go --port 3000 --api-key sk-1234567890

test:
	go test ./...

clean:
	rm -rf bin/

tidy:
	go mod tidy
	go mod download

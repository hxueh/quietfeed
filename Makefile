.PHONY: build test coverage fmt

build:
	go build -buildvcs=false -trimpath -ldflags='-s -w' -o bin/quietfeed .

test:
	go test ./...

coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

fmt:
	gofmt -w *.go

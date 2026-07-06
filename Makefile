BINARY := tele-drop
PKG    := github.com/phpgao/teledrop
CONFIG ?= config.yaml

.PHONY: build run test vet fmt tidy clean docker

build:
	go build -o bin/$(BINARY) .

run:
	go run . -config $(CONFIG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

clean:
	rm -rf bin

docker:
	docker build -t $(BINARY):latest .

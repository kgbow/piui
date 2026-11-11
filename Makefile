APP_NAME := PiUI
BIN_NAME := piui
DIST_DIR := dist

.PHONY: all build test vet app-macos clean

all: vet test build

build:
	go build -trimpath -ldflags="-s -w" -o $(BIN_NAME) .

test:
	go test -race ./...

vet:
	go vet ./...

app-macos:
	./scripts/package_macos.sh

clean:
	rm -rf $(BIN_NAME) $(DIST_DIR)

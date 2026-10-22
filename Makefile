PREFIX ?= /usr/local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet check install clean
build:
	go build -ldflags "-X main.version=$(VERSION)" -o adbmount .
vet:
	go vet ./...
test:
	go test ./... -count=1
check: vet test build
install: build
	install -d $(PREFIX)/bin
	install -m 755 adbmount $(PREFIX)/bin/
clean:
	rm -f adbmount

PREFIX ?= /usr/local

.PHONY: build test vet check install
build:
	go build -o adbfs .
vet:
	go vet ./...
test:
	go test ./... -count=1
check: vet test build
# adbmount looks for adbfs next to itself, so both go to the same directory.
install: build
	install -d $(PREFIX)/bin
	install -m 755 adbfs adbmount $(PREFIX)/bin/

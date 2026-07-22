BINARY  := horizon
PREFIX  ?= /usr/local

.PHONY: build test vet fmt install uninstall clean

build:
	go build -trimpath -ldflags "-s -w" -o $(BINARY) .

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

install: build
	install -d $(PREFIX)/bin
	install -m 0755 $(BINARY) $(PREFIX)/bin/$(BINARY)

uninstall:
	rm -f $(PREFIX)/bin/$(BINARY)

clean:
	rm -f $(BINARY)
	rm -rf dist

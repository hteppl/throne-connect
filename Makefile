VERSION ?= $(shell date +%Y.%m.%d)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build dist xray clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/throne-connect ./cmd/throne-connect

dist:
	./scripts/dist.sh $(VERSION)

xray:
	./scripts/get-xray.sh bin

clean:
	rm -rf bin/throne-connect dist

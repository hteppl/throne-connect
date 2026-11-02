VERSION ?= $(shell date +%Y.%m.%d)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build dist xray clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/throne-connect ./cmd/throne-connect

dist:
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/linux-amd64/throne-connect ./cmd/throne-connect
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/linux-arm64/throne-connect ./cmd/throne-connect
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/windows-amd64/throne-connect.exe ./cmd/throne-connect
	cp scripts/get-xray.ps1 deploy/install-windows.ps1 dist/windows-amd64/
	cp scripts/get-xray.sh deploy/throne-connect.service dist/linux-amd64/
	cp scripts/get-xray.sh deploy/throne-connect.service dist/linux-arm64/

xray:
	./scripts/get-xray.sh bin

clean:
	rm -rf bin/throne-connect dist

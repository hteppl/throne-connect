FROM golang:1.27-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/throne-connect ./cmd/throne-connect

FROM alpine:3.24 AS xray
ARG XRAY_VERSION=latest
ARG TARGETARCH
RUN apk add --no-cache curl unzip \
 && case "$TARGETARCH" in \
      amd64) asset=Xray-linux-64.zip ;; \
      arm64) asset=Xray-linux-arm64-v8a.zip ;; \
      *) echo "unsupported arch: $TARGETARCH" >&2; exit 1 ;; \
    esac \
 && if [ "$XRAY_VERSION" = latest ]; then \
      url=https://github.com/XTLS/Xray-core/releases/latest/download/$asset; \
    else \
      url=https://github.com/XTLS/Xray-core/releases/download/$XRAY_VERSION/$asset; \
    fi \
 && curl -fL --retry 3 -o /tmp/xray.zip "$url" \
 && mkdir /out && unzip -q /tmp/xray.zip xray -d /out

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata \
 && mkdir -m 1777 /data
COPY --from=build /out/throne-connect /usr/local/bin/throne-connect
COPY --from=xray /out/xray /usr/local/bin/xray
COPY docker/config.json /etc/throne-connect/config.json
VOLUME /data
ENTRYPOINT ["throne-connect"]
CMD ["run", "-c", "/etc/throne-connect/config.json"]

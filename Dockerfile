# Go 生产版打包成单二进制镜像。
# 用 alpine 是因为要访问 HTTPS 上游，需要根证书；二进制本身是纯静态的。
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go/go.mod ./
COPY go/*.go ./
# 纯标准库，无需 go mod download
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/astudio2api .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && \
    adduser -D -u 10001 astudio && \
    mkdir -p /data/accounts && chown -R astudio:astudio /data
COPY --from=build /out/astudio2api /usr/local/bin/astudio2api
USER astudio
ENV ASTUDIO_ACCOUNTS_DIR=/data/accounts \
    ASTUDIO_STATE_PATH=/data/state.json \
    ASTUDIO_HOST=0.0.0.0 \
    ASTUDIO_PORT=8788
VOLUME ["/data"]
EXPOSE 8788
ENTRYPOINT ["/usr/local/bin/astudio2api"]
CMD ["serve", "--host", "0.0.0.0", "--port", "8788"]

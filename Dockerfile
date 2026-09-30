FROM golang:1.25.1-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY public ./public
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/cline-pass-switcher .

FROM alpine:3.21
WORKDIR /app
COPY --from=builder /out/cline-pass-switcher /usr/local/bin/cline-pass-switcher
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 cline \
    && adduser -S -D -H -u 10001 -G cline cline \
    && mkdir -p /data \
    && chown 10001:10001 /data
ENV DATA_DIR=/data
ENV BIND_HOST=0.0.0.0
ENV PORT=3123
VOLUME /data
EXPOSE 3123
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/cline-pass-switcher"]

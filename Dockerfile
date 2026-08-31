# pyrite — a single static binary, so the runtime image is tiny.
#
#   docker build -t pyrite .
#   docker run --rm -p 8080:8080 pyrite serve --addr 0.0.0.0:8080 --offline
#
# Or with real data and a model, keeping the caches between runs:
#
#   docker run --rm -p 8080:8080 \
#     -e OPENAI_API_KEY \
#     -v pyrite-data:/data \
#     pyrite serve --addr 0.0.0.0:8080

FROM golang:1.27-alpine AS build
WORKDIR /src

# Dependencies first, so a source-only change reuses the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/pyrite ./cmd/pyrite

# The tests need no network and no keys, so running them here means a built
# image is a tested image.
RUN go test ./...

FROM alpine:3.20
# Certificates are the one runtime dependency: every data source and model
# provider is reached over HTTPS.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 nq \
 && mkdir -p /data && chown nq /data

COPY --from=build /out/pyrite /usr/local/bin/pyrite

USER nq
ENV PYRITE_DATA_DIR=/data
VOLUME /data
EXPOSE 8080

# Bind to all interfaces by default: the host-only default that is right for a
# laptop makes a container unreachable.
ENTRYPOINT ["pyrite"]
CMD ["serve", "--addr", "0.0.0.0:8080"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/api/health >/dev/null || exit 1

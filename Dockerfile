ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS builder

ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# -trimpath for reproducible builds; -s -w drops the symbol table/DWARF.
# Build metadata is injected into the var block in main.go.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT_SHA} -X main.buildDate=${BUILD_DATE}" \
    -o /pushward-mcp .

# distroless/static:nonroot — runs as uid 65532, no shell/package manager.
# Ships the CA bundle needed for outbound HTTPS to api.pushward.app and to fetch
# Client ID Metadata Documents during the OAuth flow. The MCP binary is a static
# CGO_ENABLED=0 build and is configured entirely via environment variables.
FROM gcr.io/distroless/static:nonroot

ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="pushward-mcp" \
      org.opencontainers.image.description="PushWard remote MCP server (Streamable HTTP, OAuth 2.1)" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT_SHA}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-mcp" \
      org.opencontainers.image.licenses="MIT"

COPY --from=builder /pushward-mcp /pushward-mcp

USER nonroot:nonroot
# 8080: MCP Streamable HTTP + OAuth endpoints. 9090: private Prometheus metrics.
EXPOSE 8080 9090

ENTRYPOINT ["/pushward-mcp"]

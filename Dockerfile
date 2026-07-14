# Stage 1: Build the management web UI (React/Vite single-file bundle)
FROM node:23-alpine AS webui

WORKDIR /webui

COPY webui/package.json webui/package-lock.json* ./
RUN npm install --no-audit --no-fund

COPY webui/ ./
RUN npm run build

# Stage 2: Build the Go binary with the embedded management UI
FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Bake the freshly built web UI into the embedded asset before compiling.
# This is the authoritative panel for the image: the server serves
# internal/managementasset/management.html when no on-disk override exists.
# .dockerignore excludes static/ so a stale host static/management.html can
# never shadow this bundled copy, and the copied config.example.yaml sets
# disable-auto-update-panel: true so the runtime auto-updater won't pull a
# remote panel over the baked one.
COPY --from=webui /webui/dist/index.html ./internal/managementasset/management.html

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

# Stage 3: Runtime image
FROM alpine:3.23

RUN apk add --no-cache tzdata

RUN mkdir -p /CLIProxyAPI/data

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.example.yaml /CLIProxyAPI/config.yaml
COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

# Enable SQLite-backed token store and usage-history by default so config and
# usage records survive container restarts without an external database.
# usg.db and the token store both live under WRITABLE_PATH (default: work dir).
ENV SQLITESTORE_ENABLED=true \
    USAGE_HISTORY_SQLITE_ENABLED=true \
    WRITABLE_PATH=/CLIProxyAPI/data

CMD ["./CLIProxyAPI"]

# Stage 1: Build React frontend
FROM node:22-slim AS frontend
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ .
RUN npm run build

# Stage 2: Build Go backend
FROM golang:1.24-bookworm AS backend
WORKDIR /app
RUN apt-get update && apt-get install -y gcc musl-dev && rm -rf /var/lib/apt/lists/*
ENV GOPROXY=https://proxy.golang.org,direct
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Copy frontend build into the embed location
COPY --from=frontend /app/web/dist ./cmd/server/web/dist
RUN CGO_ENABLED=1 go build -tags embed -ldflags='-s -w' -o /cryp ./cmd/server/

# Stage 3: Runtime
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y ca-certificates ffmpeg libheif-examples && rm -rf /var/lib/apt/lists/*
RUN useradd -m -s /bin/bash cryp
WORKDIR /app

COPY --from=backend /cryp /app/cryp
COPY LICENSE /licenses/Cryp/LICENSE

RUN mkdir -p /data/config /data/vaults && chown -R cryp:cryp /data

USER cryp

EXPOSE 9527

VOLUME ["/data"]

ENV PORT=9527
ENV DATA_DIR=/data/config
ENV VAULT_DIR=/data/vaults
# SOURCE_DIR defaults to /data; set it to a dedicated media subtree in
# deployments that share the mount with other application data.
ENV SOURCE_DIR=/data

CMD ["/app/cryp"]

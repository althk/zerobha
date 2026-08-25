# --- Build Stage ---
FROM golang:1.24-alpine AS builder

WORKDIR /src
RUN apk add --no-cache git

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Build binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /bin/trader ./cmd/trader

# --- Runtime Stage ---
FROM alpine:3.20

WORKDIR /app

# Install tzdata (mandatory for IST market hours), ca-certificates (for HTTPS Kite API), and sqlite
RUN apk add --no-cache tzdata ca-certificates sqlite rclone bash

# Set IST Timezone
ENV TZ=Asia/Kolkata
RUN cp /usr/share/zoneinfo/Asia/Kolkata /etc/localtime && echo "Asia/Kolkata" > /etc/timezone

# Copy binary and configuration files
COPY --from=builder /bin/trader /app/trader
COPY indices.csv /app/indices.csv
COPY config.local.toml /app/config.local.toml

# Set working directory to data mount so zerobha.db is persisted automatically
WORKDIR /app/data

VOLUME ["/app/logs", "/app/data"]

ENTRYPOINT ["/app/trader"]
# The strategy is selected by the `strategy` key in the baked config, not by a
# flag: cmd/trader defines only -config and -paper, and an unknown flag makes
# flag.Parse exit before the trader starts. Add "-paper" here (or set
# paper_trading = true in the config) to run the container in paper mode.
CMD ["-config", "/app/config.local.toml"]

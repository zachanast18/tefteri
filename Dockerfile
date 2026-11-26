# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ledger ./cmd/ledger

FROM debian:bookworm-slim AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    tzdata \
    tesseract-ocr \
    tesseract-ocr-ell \
    tesseract-ocr-eng \
    && rm -rf /var/lib/apt/lists/* \
    && tesseract --list-langs | grep -q '^ell$' \
    && tesseract --list-langs | grep -q '^eng$'

RUN groupadd --system --gid 10001 ledger \
    && useradd --system --uid 10001 --gid 10001 --home-dir /data --shell /usr/sbin/nologin ledger \
    && mkdir -p /data \
    && chown 10001:10001 /data

COPY --from=builder /out/ledger /usr/local/bin/ledger

USER 10001:10001
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080

ENV LISTEN_ADDR=":8080" \
    DB_PATH="/data/ledger.db" \
    APP_CURRENCY="EUR" \
    APP_TIMEZONE="Europe/Athens" \
    OCR_LANGUAGES="ell+eng" \
    COOKIE_SECURE="auto"

ENTRYPOINT ["/usr/local/bin/ledger"]
CMD ["serve"]

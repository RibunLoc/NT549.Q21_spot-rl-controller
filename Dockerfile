# ── Stage 1: Build ──────────────────────────────────────────────────────────
# Dùng golang image có sẵn gcc (cgo cần thiết cho onnxruntime_go)
FROM golang:1.24-bookworm AS builder

WORKDIR /src

# Tải ONNX Runtime cho Linux x64
ARG ONNX_VERSION=1.20.1
RUN apt-get update -qq && apt-get install -y --no-install-recommends wget ca-certificates && \
    wget -q "https://github.com/microsoft/onnxruntime/releases/download/v${ONNX_VERSION}/onnxruntime-linux-x64-${ONNX_VERSION}.tgz" \
         -O /tmp/ort.tgz && \
    tar -xzf /tmp/ort.tgz -C /tmp && \
    mkdir -p /opt/onnxruntime && \
    cp -r /tmp/onnxruntime-linux-x64-${ONNX_VERSION}/lib   /opt/onnxruntime/ && \
    cp -r /tmp/onnxruntime-linux-x64-${ONNX_VERSION}/include /opt/onnxruntime/ && \
    rm -rf /tmp/ort.tgz /tmp/onnxruntime-linux-x64-*

# Cache go modules trước
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build với CGO, trỏ tới ORT headers + libs
ENV CGO_ENABLED=1 \
    CGO_CFLAGS="-I/opt/onnxruntime/include" \
    CGO_LDFLAGS="-L/opt/onnxruntime/lib -lonnxruntime" \
    LD_LIBRARY_PATH=/opt/onnxruntime/lib

RUN go build -tags cgo -o /controller ./cmd/controller

# ── Stage 2: Runtime ─────────────────────────────────────────────────────────
FROM debian:bookworm-slim

# ORT shared lib cần tại runtime
COPY --from=builder /opt/onnxruntime/lib/libonnxruntime.so* /usr/local/lib/
RUN ldconfig

COPY --from=builder /controller /usr/local/bin/controller

# Model và config được mount vào /opt/spot-rl
WORKDIR /opt/spot-rl

# Health check đơn giản: process còn chạy không
HEALTHCHECK --interval=60s --timeout=5s --retries=3 \
    CMD pgrep controller || exit 1

ENTRYPOINT ["/usr/local/bin/controller"]

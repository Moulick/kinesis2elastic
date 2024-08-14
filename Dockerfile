FROM docker.io/library/golang:1.23 as builder

WORKDIR /app
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download -x

# Copy the go source
COPY gzipbinding/ gzipbinding/
COPY incoming/ incoming/
COPY outgoing/ outgoing/
COPY log/ log/
COPY main.go main.go
COPY ingest/ ingest/

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o kinesis2elastic main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM scratch
LABEL org.opencontainers.image.source="https://github.com/Moulick/kinesis2elastic"
LABEL org.opencontainers.image.url="https://github.com/Moulick/kinesis2elastic"
LABEL org.opencontainers.image.licenses="Apache-2.0"
LABEL org.opencontainers.image.title="Kinesis2Elastic"
LABEL org.opencontainers.image.base.name="dockerhub.io/moulick/kinesis2elastic:latest"
COPY --from=builder /app/kinesis2elastic /
ENTRYPOINT ["/kinesis2elastic"]

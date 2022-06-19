FROM docker.io/library/golang:1.18 as builder
MAINTAINER Moulick Aggarawl <moulickaggarwal@gmail.com>

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
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /app/kinesis2elastic .
USER nonroot:nonroot

ENTRYPOINT ["/kinesis2elastic"]

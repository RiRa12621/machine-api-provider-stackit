FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.3 AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -mod=readonly -trimpath -buildvcs=false -ldflags='-s -w' -o /out/machine-controller-manager ./cmd/machine-controller-manager

FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.source="https://github.com/RiRa12621/machine-api-provider-stackit"
LABEL org.opencontainers.image.licenses="Apache-2.0"
# Include the additional trust bundle mounted by MAO alongside the system CAs.
ENV SSL_CERT_DIR=/etc/ssl/certs:/etc/pki/ca-trust/extracted/pem
COPY --from=builder /out/machine-controller-manager /machine-controller-manager
USER 65532:65532
ENTRYPOINT ["/machine-controller-manager"]

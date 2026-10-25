FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN test -s /etc/ssl/certs/ca-certificates.crt \
    && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
        go build -buildvcs=false -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/arrtify ./cmd/arrtify

FROM scratch
COPY --from=build /out/arrtify /arrtify
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
USER 65532:65532
ENTRYPOINT ["/arrtify"]

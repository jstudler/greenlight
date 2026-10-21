# Multi-stage build: compile with the Go toolchain, ship a distroless image.
FROM golang:1.26 AS build

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags="-s -w -X github.com/jstudler/greenlight/internal/app.Version=${VERSION}" \
    -o /out/greenlight ./cmd/greenlight

FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=build /out/greenlight /greenlight
COPY examples/ /etc/greenlight/examples/

USER 65532:65532
ENTRYPOINT ["/greenlight"]

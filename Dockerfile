# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26.4

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/moment-agent ./cmd/moment-agent

FROM busybox:1.36 AS dirs
RUN mkdir -p \
    /usr/local/share/xray \
    /var/lib/moment/xray-agent \
    /var/log/moment/xray-agent

FROM gcr.io/distroless/base-debian12:nonroot
WORKDIR /app

COPY --from=build /out/moment-agent /usr/local/bin/moment-agent
COPY --from=dirs --chown=nonroot:nonroot /usr/local/share/xray /usr/local/share/xray
COPY --from=dirs --chown=nonroot:nonroot /var/lib/moment/xray-agent /var/lib/moment/xray-agent
COPY --from=dirs --chown=nonroot:nonroot /var/log/moment/xray-agent /var/log/moment/xray-agent

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/moment-agent"]

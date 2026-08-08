ARG BUILDER_IMAGE=golang:1.25.0-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM --platform=$BUILDPLATFORM ${BUILDER_IMAGE} AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY LICENSE ./LICENSE
COPY cmd ./cmd
COPY pkg ./pkg

ARG TARGETOS=linux
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -mod=readonly -trimpath -ldflags="-s -w" \
    -o /out/neocloud-node ./cmd/neocloud-node

FROM ${RUNTIME_IMAGE}

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="AIBrix NeoCloud Node" \
      org.opencontainers.image.description="Kubernetes bridge to NeoCloud GPU capacity" \
      org.opencontainers.image.source="https://github.com/aibrix/neocloud-node" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=builder --chown=nonroot:nonroot /out/neocloud-node /neocloud-node
COPY --from=builder /src/LICENSE /licenses/neocloud-node/LICENSE

USER nonroot:nonroot
EXPOSE 10250
STOPSIGNAL SIGTERM
ENTRYPOINT ["/neocloud-node"]

# Two stages: compile a static Go binary, then copy it into an empty
# (distroless) image. Result: one ~10 MB image, no shell, no runtime deps.
#   docker build -t kizuna .
# GO_VERSION follows the go line in go.mod: the docker workflow passes it in,
# and ci fails if this default drifts from it.
ARG GO_VERSION=1.27
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY . .
RUN go test ./... \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X kizuna/internal/provider.Version=${VERSION}" -o /out/kizuna ./cmd/kizuna \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/kizuna /kizuna
COPY --from=build --chown=1000:1000 /out/data /data
ENV DATA_DIR=/data PORT=32600
VOLUME /data
EXPOSE 32600
USER 1000:1000
HEALTHCHECK --interval=60s --timeout=5s --start-period=60s CMD ["/kizuna", "health"]
ENTRYPOINT ["/kizuna"]

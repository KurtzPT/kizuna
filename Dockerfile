# Two stages: compile a static Go binary, then copy it into an empty
# (distroless) image. Result: one ~10 MB image, no shell, no runtime deps.
#   docker build -t kizuna .
# GO_VERSION follows the go line in go.mod: the docker workflow passes it in,
# and ci fails if this default drifts from it.
ARG GO_VERSION=1.27
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY . .
# before the ARGs: they'd make this step one per target platform, but the
# tests run on the build platform, so one run (a shared layer) is enough
RUN go test ./...
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X kizuna/internal/provider.Version=${VERSION}" -o /out/kizuna ./cmd/kizuna \
 && mkdir -p /out/data

# pinned by digest so an image rebuild is reproducible; Dependabot opens a pull
# request against dev when Google publishes a new one
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/kizuna /kizuna
COPY --from=build --chown=1000:1000 /out/data /data
# no PORT here: the default is 32600, and PORT in kizuna.env must work too
ENV DATA_DIR=/data
VOLUME /data
EXPOSE 32600
USER 1000:1000
HEALTHCHECK --interval=60s --timeout=5s --start-period=60s CMD ["/kizuna", "health"]
ENTRYPOINT ["/kizuna"]

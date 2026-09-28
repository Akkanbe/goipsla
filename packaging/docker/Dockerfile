# goipsla container image: goipslad (the daemon, the entry point) and goipsla
# (the CLI). Build from the repository root:
#
#   make image                       # goipsla:dev
#   docker build -f packaging/docker/Dockerfile --build-arg VERSION=1.2.3 -t goipsla:1.2.3 .
#
# Run (see packaging/docker/README.md):
#
#   docker run -d --name goipslad --network host \
#     --cap-drop ALL --cap-add NET_RAW \
#     -v /etc/goipslad:/etc/goipslad:ro -v /run/goipslad:/run/goipslad \
#     goipsla:dev

# Both base images are pinned by digest (multi-arch OCI index) so that the
# same sources and VERSION give the same image. The tag is informational;
# the digest decides. To update them, see "Updating the base images" in
# packaging/docker/README.md.
FROM golang:1.26.8@sha256:6c2a5538f964f1c82f97ad14988bf05de100d922d159d0e398b54c7b0ca0c6c9 AS build
ARG VERSION=0.0.0-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/ ./cmd/goipslad ./cmd/goipsla \
 && mkdir -p /out/etc/goipslad /out/run/goipslad

# Static binaries only: no shell, no package manager.
FROM gcr.io/distroless/static-debian12:latest@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
COPY --from=build /out/goipslad /out/goipsla /
COPY --from=build /out/etc/goipslad /etc/goipslad
COPY --from=build /out/run/goipslad /run/goipslad
COPY packaging/config.example.yaml /etc/goipslad/config.example.yaml
# The process runs as the container's root so that --cap-add NET_RAW reaches
# it (Docker does not give a non-root user ambient capabilities). Drop every
# other capability at run time with --cap-drop ALL.
ENTRYPOINT ["/goipslad"]
CMD ["--config", "/etc/goipslad/config.yaml"]

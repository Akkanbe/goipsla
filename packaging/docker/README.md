# Running goipslad in a container image

`packaging/docker/Dockerfile` builds an image containing only the statically linked `goipslad` (the entry point) and `goipsla` (the CLI). The base is `gcr.io/distroless/static-debian12`, with no shell and no package manager (about 37 MB).

## Build

From the repository root:

```sh
make image                             # goipsla:dev
make image VERSION=1.2.3 IMAGE_TAG=1.2.3
docker run --rm goipsla:dev --version  # goipslad 1.2.3 (go1.26.x linux/amd64)
```

The build context is narrowed to the Go sources and the configuration example by `packaging/docker/Dockerfile.dockerignore` (this requires BuildKit; `make image` sets `DOCKER_BUILDKIT=1`).

## Run

```sh
sudo install -d -m 0755 /etc/goipslad /run/goipslad
sudo cp packaging/config.example.yaml /etc/goipslad/config.yaml   # edit it
docker run --rm -v /etc/goipslad:/etc/goipslad:ro --entrypoint /goipsla goipsla:dev \
  validate /etc/goipslad/config.yaml

docker run -d --name goipslad --restart unless-stopped \
  --network host \
  --cap-drop ALL --cap-add NET_RAW \
  -v /etc/goipslad:/etc/goipslad:ro \
  -v /run/goipslad:/run/goipslad \
  goipsla:dev
```

- **`--network host`**: used to measure over the host's interfaces, routing tables, and VRF devices. The image also works on a bridge network, but then the source is the container's address, and `source-interface` and `vrf` refer to the container's own.
- **`--cap-drop ALL --cap-add NET_RAW`**: the only privilege required is `CAP_NET_RAW` (for the raw ICMP sockets and `SO_BINDTODEVICE` to a VRF).
- **Why it runs as root inside the container**: Docker does not give ambient capabilities to non-root users, so with `--user` the `CAP_NET_RAW` capability does not reach the process and startup fails with `need CAP_NET_RAW`. The process therefore runs as the container's root, and `--cap-drop ALL` drops everything except `NET_RAW`.
- **`/run/goipslad`**: mount this when you want to use the API socket (`/run/goipslad/goipslad.sock`) from `goipsla` on the host. It is not needed if you only use `goipsla` inside the container.
- **Location of the configuration example**: it is at `/etc/goipslad/config.example.yaml` inside the image. Mounting `/etc/goipslad` hides it.

## Operation

```sh
docker exec goipslad /goipsla show operations
docker exec goipslad /goipsla health
docker kill --signal HUP goipslad      # reload the configuration (same as goipsla reload)
docker logs -f goipslad
```

`goipsla` is included in the image, so you can use it through `docker exec` without installing it on the host.

## Updating the base images

The Dockerfile pins the builder (`golang`) and the runtime image (`gcr.io/distroless/static-debian12`) by digest. The same sources and `VERSION` produce an image with the same contents, and the build result does not change when upstream tags are updated. In exchange, Go updates and security fixes in the base image are taken in by updating the digests by hand.

| Image | Pinned value (2026-09-27) |
|---|---|
| Builder | `golang:1.26.8@sha256:6c2a5538f964f1c82f97ad14988bf05de100d922d159d0e398b54c7b0ca0c6c9` |
| Runtime | `gcr.io/distroless/static-debian12:latest@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2` |

Both are digests of multi-architecture indexes (OCI image index), so the same lines build on both amd64 and arm64.

Update procedure:

1. Look up the index digest of the new tag (the `Digest:` line).

   ```sh
   docker buildx imagetools inspect golang:1.26
   docker buildx imagetools inspect gcr.io/distroless/static-debian12
   ```

   `docker image inspect --format '{{index .RepoDigests 0}}' <image>` after `docker pull` gives the same value. Use the top-level `Digest:`, not the digests of the per-platform manifests (each line under `Manifests:` in `imagetools inspect`).
2. Rewrite the digests of the two `FROM` lines in `packaging/docker/Dockerfile` (and, for the builder, the tag that indicates the Go version), and the table above. The builder's Go version must be at least the `go` line of `go.mod`.
3. Build with `make image` and check the Go version with `docker run --rm goipsla:dev --version`.
4. If possible, check that measurement actually works with `--network host --cap-drop ALL --cap-add NET_RAW` (the same steps as in "What was verified" below).

Follow this procedure when a Go security fix (patch release) is published, and when taking in distroless updates (CA certificates, tzdata, and so on; goipslad uses outbound TLS only for webhooks).

## What was verified

- The image builds with `make image`, and `docker run --rm goipsla:dev --version` works.
- With `--network host --cap-drop ALL --cap-add NET_RAW`, icmp-echo and icmp-jitter operations can measure, and their state is visible with `docker exec ... /goipsla show operations` and `health` (against the staging targets 10.100.1.11 / .12).
- Without `NET_RAW`, it stops with `goipslad: cannot start the ICMP engine (need CAP_NET_RAW): probe: open icmp4 raw socket: operation not permitted`.

# crowdvm-server image (amd64 and arm64, e.g. a Raspberry Pi 5).
#
#   docker build -t crowdvm-server .                     # on the Pi itself
#   docker buildx build --platform linux/arm64 -t crowdvm-server .   # cross-build
#
# The build runs on the build machine's own platform and cross-compiles, so
# an arm64 image builds at native speed on an x86 machine.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine AS build
ARG TARGETOS TARGETARCH VERSION=""
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# VERSION defaults to the repository's VERSION file.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/crowdvm/crowdvm/internal/server.Version=${VERSION:-$(cat VERSION)}" \
    -o /out/crowdvm-server ./cmd/crowdvm-server
RUN mkdir -p /out/data

# distroless/static: CA certificates and time zones, no shell, ~2 MB.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/crowdvm-server /usr/local/bin/crowdvm-server
# /data belongs to the image's user, so a new named volume starts writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENV CROWDVM_SERVER_DATA=/data
VOLUME /data
EXPOSE 8420
ENTRYPOINT ["crowdvm-server"]
CMD ["serve", "--listen", ":8420"]

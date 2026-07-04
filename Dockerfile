# scrim hub container image. Runs `scrim hub` -- the same serving engine as
# the local daemon, at its own durable data volume, gated by a required push
# token + CIDR read allowlist (see internal/server's hub mode). This image is
# hub-only: the local CLI (add/link/serve/...) is meant to run on a
# developer's own machine, not in a container.
#
# ARG VERSION/COMMIT/DATE mirror the Makefile's -ldflags stamping (see
# internal/version) so a container-built binary reports the same version
# info `make build` would.
ARG VERSION=docker
ARG COMMIT=unknown
ARG DATE=

FROM golang:1.26 AS builder
ARG VERSION
ARG COMMIT
ARG DATE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-X github.com/jedwards1230/scrim/internal/version.Version=${VERSION} -X github.com/jedwards1230/scrim/internal/version.Commit=${COMMIT} -X github.com/jedwards1230/scrim/internal/version.Date=${DATE}" \
    -o /out/scrim .
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/scrim /usr/local/bin/scrim
COPY --from=builder --chown=65532:65532 /data /data

ENV SCRIM_HUB_DATA=/data
VOLUME /data
EXPOSE 7788

USER nonroot

# ENTRYPOINT fixes the verb; CMD is empty so `docker run <image> --allow ...
# --push-token ...` appends flags rather than replacing the whole command
# line.
ENTRYPOINT ["/usr/local/bin/scrim", "hub"]
CMD []

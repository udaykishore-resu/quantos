# Multi-stage build shared by every QuantOS service.
#
# The final image is distroless and contains one static binary plus the
# configuration it reads. No shell, no package manager, no source: the smallest
# surface that can still do the job.

ARG GO_VERSION=1.24

# --------------------------------------------------------------- build -----
FROM golang:${GO_VERSION}-bookworm AS build

WORKDIR /src

# Dependencies first, so a source change does not re-download the module graph.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# SERVICE names either one of the nine role binaries under services/, or
# "quantos" for the all-in-one command under cmd/. The second is what the local
# Kubernetes overlay runs: the split topology needs a broker to carry events
# between pods, and until drivers/kafka exists the honest single-node
# deployment is one process running every role.
ARG SERVICE=api-gateway
ARG VERSION=dev
ARG COMMIT=unknown

# CGO off gives a static binary; trimpath removes local filesystem paths from
# the build, which is both smaller and one less thing leaked in a stack trace.
#
# The cache mounts matter locally: nine services are built from this same file,
# and without them each one recompiles the whole dependency graph from scratch.
# With them the first build is slow and the other eight are not.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w \
        -X github.com/udaykishoreresu/quantos/internal/cli.buildVersion=${VERSION} \
        -X github.com/udaykishoreresu/quantos/internal/cli.buildCommit=${COMMIT}" \
      -o /out/service "$(test "${SERVICE}" = quantos && echo ./cmd/quantos || echo ./services/${SERVICE})"

# Verify the binary starts. A build that produces something unrunnable should
# fail here, not in a cluster. The two shapes take the flag differently: the
# role binaries parse flags directly, the multiplexer takes a subcommand first.
RUN if [ "${SERVICE}" = quantos ]; then /out/service run -version; else /out/service -version; fi

# ------------------------------------------------------------- runtime -----
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/service /app/service
COPY --from=build /src/strategies /app/strategies
COPY --from=build /src/config /etc/quantos

# The model artifact travels with the image.
#
# Compose bind-mounts ml/artifacts from the host, so the omission was invisible
# there. In Kubernetes there is no host to mount from: without this the registry
# finds no artifact, every service falls back to the deterministic rule prior,
# and the platform reports itself degraded for a reason nobody would guess from
# the logs. mlinfer refuses to load an artifact that does not carry its own
# held-out report card, so shipping it is also shipping the evidence for it.
COPY --from=build /src/ml/artifacts /app/ml/artifacts

# Distroless nonroot is uid 65532.
USER nonroot:nonroot

ENV QUANTOS_CONFIG=/etc/quantos/quantos.compose.yaml

EXPOSE 8080 9090

ENTRYPOINT ["/app/service"]

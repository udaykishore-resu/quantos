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
RUN go mod download

COPY . .

ARG SERVICE=api-gateway
ARG VERSION=dev
ARG COMMIT=unknown

# CGO off gives a static binary; trimpath removes local filesystem paths from
# the build, which is both smaller and one less thing leaked in a stack trace.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w \
        -X github.com/udaykishoreresu/quantos/internal/cli.buildVersion=${VERSION} \
        -X github.com/udaykishoreresu/quantos/internal/cli.buildCommit=${COMMIT}" \
      -o /out/service ./services/${SERVICE}

# Verify the binary starts and can print its configuration. A build that
# produces something unrunnable should fail here, not in a cluster.
RUN /out/service -version

# ------------------------------------------------------------- runtime -----
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/service /app/service
COPY --from=build /src/strategies /app/strategies
COPY --from=build /src/config /etc/quantos

# Distroless nonroot is uid 65532.
USER nonroot:nonroot

ENV QUANTOS_CONFIG=/etc/quantos/quantos.compose.yaml

EXPOSE 8080 9090

ENTRYPOINT ["/app/service"]

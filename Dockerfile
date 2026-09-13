# Multi-stage: a Go binary needs no runtime, so the final image carries nothing
# but the executable and CA certificates. ~15MB, versus ~1GB for a Node image
# with node_modules — which matters on a platform that scales to zero and pays
# for the cold start on every wake.

FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first: this layer is cached until go.mod/go.sum actually change,
# so ordinary source edits skip the download entirely.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Version and commit are injected at link time and surface on /health. Both
# existing services in this org can report neither, which has cost real time
# working out what is actually deployed.
ARG VERSION=dev
ARG COMMIT=unknown

# CGO_ENABLED=0 produces a static binary, which is what lets the final stage be
# a scratch-like image with no libc.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w \
            -X github.com/HoseaCodes/arcade-api/internal/config.Version=${VERSION} \
            -X github.com/HoseaCodes/arcade-api/internal/config.Commit=${COMMIT}" \
        -o /out/server ./cmd/server

# distroless/static: no shell, no package manager, no libc. Nothing for an
# attacker who lands here to pivot with.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/server /server

# Non-root by default in this base image. Nothing here needs to write to disk.
USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/server"]

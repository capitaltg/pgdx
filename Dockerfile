# syntax=docker/dockerfile:1

# --- Build stage -------------------------------------------------------------
# Run the build on the native runner arch (BUILDPLATFORM) and cross-compile to
# the requested target — avoids slow QEMU emulation for multi-arch builds.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

# Version stamped into the binary (matches the Makefile's -ldflags target).
ARG VERSION=dev
# Provided automatically by BuildKit for the target platform.
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Cache module downloads separately from the source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build a static binary so it runs in a scratch image.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w -X github.com/capitaltg/pgdx/cmd.version=${VERSION}" \
    -o /out/pgdx .

# --- Runtime stage -----------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/pgdx /usr/local/bin/pgdx

ENTRYPOINT ["pgdx"]

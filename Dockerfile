# Multi-stage build, parameterized by service so every Go binary in cmd/ shares
# one Dockerfile. Build with: docker build --build-arg SERVICE=payments-api .
ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS build
ARG SERVICE
WORKDIR /src

# Dependencies are copied and downloaded first so this layer stays cached
# across source edits — the slow step only reruns when go.mod changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a static binary, which is what lets the final stage be
# a distroless image with no libc at all.
# -trimpath keeps build-host paths out of the binary and makes builds reproducible.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags="-s -w" \
        -o /out/service ./cmd/${SERVICE}

# A tiny static healthcheck binary, because distroless has no curl or wget and
# Compose needs something to exec.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
        -o /out/healthcheck ./cmd/healthcheck

# distroless rather than alpine: no shell and no package manager means a
# compromised service has nothing to pivot with. It also runs as nonroot by
# default, which matters for a service handling card data (§13).
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/service /app/service
COPY --from=build /out/healthcheck /app/healthcheck
USER nonroot:nonroot
ENTRYPOINT ["/app/service"]

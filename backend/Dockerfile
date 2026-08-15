# Stage 1: Build
FROM golang:1.26-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
# Copying go.mod + go.sum before the rest of the source means this layer only
# invalidates when dependencies change, not on every code edit — go.sum now
# exists (Phase 1 landed pgx/huma/x-image, see docs/PHASE1_BUILD_PLAN.md
# WO-1), so both must be copied for `go mod download` to verify against it
# rather than hitting the network for an unverified go.sum on every build.
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /build/api-server ./cmd/api-server

# Stage 2: Runtime — distroless, not alpine: per docs/DESIGN.md's Cloud Run
# research, image size is the biggest cold-start lever, and a slim distroless
# image measurably beats an Alpine base on Cloud Run cold starts.
#
# :nonroot rather than the default (root) tag. The binary is copied world-
# readable and executable, it binds 8080 so it needs no privileged-port
# capability, and it writes nothing to disk when object storage is configured —
# which it always is in production (internal/media/newstore.go). So the tag
# costs nothing and means a container escape starts as uid 65532 instead of 0.
# Cloud Run does not require it, which is precisely why it is easy to never do.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=builder /build/api-server .

EXPOSE 8080
ENTRYPOINT ["/app/api-server"]

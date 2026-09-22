# Multi-stage build keeps the runtime layer minimal and identical for both
# processes (I02). The digest-addressable tag is applied by CI.
FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.version=${VERSION:-0.1.0-i02}" \
    -o /out/control-plane-worker ./cmd/control-plane-worker

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/control-plane-worker /control-plane-worker
USER nonroot
EXPOSE 8081
ENTRYPOINT ["/control-plane-worker"]

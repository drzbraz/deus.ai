# syntax=docker/dockerfile:1

# ---- build: compile a static binary --------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src

# Download modules in their own layer so code changes do not refetch them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# ---- test: run the integration suite inside Docker ----------------------
FROM build AS test
ENTRYPOINT ["go", "test", "-tags=integration", "-p", "1", "-count=1", "-cover", "./..."]

# ---- runtime: minimal, non-root image ------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/api /api
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/api"]

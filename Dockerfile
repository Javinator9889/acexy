# syntax=docker/dockerfile:1

# Build the application from source
FROM golang:1.22 AS build-stage

WORKDIR     /app
COPY --link acexy/ ./

RUN go mod download

RUN CGO_ENABLED=0 GOOS=linux go build -o /acexy

# Create a minimal image
FROM martinbjeldbak/acestream-http-proxy:2.3 AS final-stage

COPY --link             bin/entrypoint /bin/entrypoint
COPY --from=build-stage /acexy         /acexy
EXPOSE 8080
ENV EXTRA_FLAGS="--cache-dir /tmp --cache-limit 2 --cache-auto 1 --log-stderr --log-stderr-level error"
ENV ACEXY_LISTEN_ADDR=":8080"
# USER acestream:acestream

# Install curl for healthcheck
RUN apt-get update && apt-get install -y curl && rm -rf /var/lib/apt/lists/*

# Healthcheck against the HTTP status endpoint
HEALTHCHECK --interval=10s --timeout=10s --start-period=1s \
    CMD curl -qf http://localhost${ACEXY_LISTEN_ADDR}/ace/status || exit 1

ENTRYPOINT [ "/bin/entrypoint" ]

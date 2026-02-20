# syntax=docker/dockerfile:1

# Build the application from source
FROM --platform=$BUILDPLATFORM golang:1.24 AS build-stage
ARG  TARGETOS
ARG  TARGETARCH

WORKDIR     /app
COPY --link acexy/ ./

RUN go mod download

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags "-s -w" -o /acexy

# Create a minimal image
FROM alpine:latest AS final-stage

COPY --from=build-stage /acexy         /acexy
EXPOSE 8080
ENV ACEXY_LISTEN_ADDR=":8080"
# USER acestream:acestream

# Install curl for healthcheck
RUN apk add --no-cache curl

# Healthcheck against the HTTP status endpoint
HEALTHCHECK --interval=10s --timeout=10s --start-period=1s \
    CMD curl -qf http://localhost${ACEXY_LISTEN_ADDR}/ace/status || exit 1

ENTRYPOINT [ "/acexy" ]

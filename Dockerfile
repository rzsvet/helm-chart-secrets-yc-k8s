# Build environment
# -----------------
FROM golang:1.25.1-alpine3.22 AS build-env
WORKDIR /helm-secrets

COPY go.mod go.sum ./
RUN go mod download

COPY ./cmd ./cmd/
RUN go build -ldflags '-w -s' -a -o ./bin/helm-secrets ./cmd/helm-secrets

# Deployment environment
# ----------------------
FROM scratch
COPY --from=build-env /helm-secrets/bin/helm-secrets .
COPY migrations /migrations
CMD ["/helm-secrets"]

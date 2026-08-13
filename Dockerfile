# ---- build stage ------------------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache modules first (no external deps, but keeps rebuilds fast).
COPY go.mod ./
RUN go mod download

COPY . .

# Fully static, stripped binary — CGO off so it links nothing from the OS.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /mavenrepo .

# Prepare a data dir owned by the non-root uid the final image runs as.
RUN mkdir -p /repo && chown 65532:65532 /repo

# ---- final stage ------------------------------------------------------------
FROM scratch

# The static binary and an empty, writable repository directory.
COPY --from=build /mavenrepo /mavenrepo
COPY --from=build --chown=65532:65532 /repo /repo

# Run as a non-root numeric user (scratch has no /etc/passwd).
USER 65532:65532

EXPOSE 8080
VOLUME ["/repo"]

# Override the CMD to add -user/-pass, change -addr, etc.:
#   docker run -p 8080:8080 -v repo:/repo hub -user deployer -pass s3cret
ENTRYPOINT ["/mavenrepo"]
CMD ["-addr", ":8080", "-root", "/repo"]

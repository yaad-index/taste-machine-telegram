# The service and the engine's compile command in one image (ADR 0001
# sections 1 and 6). The engine is installed at exactly the version go.mod
# pins, so the library the service links and the command that writes its
# files are the same release.

# The build stage runs on the builder's own platform and cross-compiles,
# so a multi-platform build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.1 AS build
ARG TARGETOS TARGETARCH
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/taste-machine-telegram ./cmd/taste-machine-telegram
# go install puts a cross-compiled binary in a GOOS_GOARCH subdirectory
# and refuses GOBIN, so the binary is copied from wherever it landed.
RUN engine="$(go list -m -f '{{.Version}}' github.com/yaad-index/taste-machine)" && \
    go install -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=${engine}" \
      "github.com/yaad-index/taste-machine/cmd/taste-machine@${engine}" && \
    bin="$(go env GOPATH)/bin" && \
    cp "$(ls "$bin/${TARGETOS}_${TARGETARCH}/taste-machine" "$bin/taste-machine" 2>/dev/null | head -n 1)" /out/
# The data directory, owned by the runtime image's non-root user.
RUN mkdir -p /data && chown 65532:65532 /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
COPY --from=build --chown=65532:65532 /data /data
ENV TASTE_MACHINE_TELEGRAM_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
USER nonroot
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
  CMD ["taste-machine-telegram", "health"]
ENTRYPOINT ["taste-machine-telegram"]
CMD ["serve"]

# A static binary on a minimal base: the runtime image needs no Go toolchain.
# The build stage runs on the builder's own platform and cross-compiles, so an
# amd64 image builds natively fast on an arm64 Mac.
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
ARG TARGETOS TARGETARCH
WORKDIR /build
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd cmd
COPY internal internal
ARG BUILD_SHA=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.Version=${BUILD_SHA}" -o /visicomms ./cmd/visicomms

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /visicomms /visicomms
ENV VISICOMMS_ENV=production PORT=8080
EXPOSE 8080
ENTRYPOINT ["/visicomms"]
CMD ["serve"]

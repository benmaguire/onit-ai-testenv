# Builds the simulator controller and the simctl CLI.
#   docker build -t ghcr.io/benmaguire/onit-ai-testenv/simulator:dev simulations/nvidia-sim
FROM --platform=$BUILDPLATFORM golang:1.24 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" -o /out/simulator ./cmd/simulator && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w" -o /out/simctl ./cmd/simctl

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/simulator /out/simctl /usr/local/bin/
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/simulator"]

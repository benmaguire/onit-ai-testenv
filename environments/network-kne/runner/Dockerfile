# kne CLI + kubectl, for creating/deleting KNE topologies from inside the cluster.
#   docker build -t ghcr.io/benmaguire/onit-ai-testenv/kne-runner:v0.3.4 environments/network-kne/runner
# Declared for builders without BuildKit (scripts/local passes it); BuildKit sets it itself.
ARG BUILDPLATFORM
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG KNE_VERSION=v0.3.4 TARGETOS TARGETARCH
RUN git clone --depth 1 --branch ${KNE_VERSION} https://github.com/openconfig/kne /src && cd /src && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/kne_cli ./kne_cli

FROM alpine:3.22
ARG TARGETARCH KUBECTL_VERSION=v1.34.2
RUN apk add --no-cache bash ca-certificates curl && \
    curl -fsSLo /usr/local/bin/kubectl https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl && \
    chmod +x /usr/local/bin/kubectl
COPY --from=build /out/kne_cli /usr/local/bin/kne
COPY kne-reconcile /usr/local/bin/kne-reconcile
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/kne-reconcile"]

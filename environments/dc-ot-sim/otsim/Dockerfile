# otsim: data centre OT plant simulator (Modbus TCP, SNMP, Redfish, metrics).
#   docker build -t ghcr.io/benmaguire/onit-ai-testenv/otsim:dev environments/dc-ot-sim/otsim
# Declared for builders without BuildKit (scripts/local passes it); BuildKit sets it itself.
ARG BUILDPLATFORM
FROM --platform=$BUILDPLATFORM golang:1.24 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/otsim ./cmd/otsim

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/otsim /usr/local/bin/otsim
COPY examples/site.yaml /etc/otsim/site.yaml
USER 65532:65532
# Unprivileged ports; the Kubernetes Services map 502 and 161 onto them.
EXPOSE 8080 8443 1502 1161/udp
ENTRYPOINT ["/usr/local/bin/otsim"]
CMD ["serve", "--modbus-gateway", "tcp://0.0.0.0:1502", "--snmp-addr", ":1161"]

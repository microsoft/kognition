# syntax=docker/dockerfile:1
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.


# --- Build stage ---
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine@sha256:9ce231b28998b32e116927c61aa5c406183061d4f3f2d61b292c88e7f6bdf7ca AS builder
ARG TARGETOS
ARG TARGETARCH
ENV GOOS=$TARGETOS GOARCH=$TARGETARCH

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go list -mod=readonly -deps ./cmd/... > /kognition.dependencies && \
    if grep -E '^golang.org/x/crypto/openpgp(/|$)' /kognition.dependencies; then exit 1; fi

RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognition-apiserver ./cmd/kognition-apiserver
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognition-controller-manager ./cmd/kognition-controller-manager
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognitionctl ./cmd/kognitionctl
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognition-k8s-controller ./cmd/kognition-k8s-controller
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognition-memory-service ./cmd/kognition-memory-service
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognition-memory-policy-gateway ./cmd/kognition-memory-policy-gateway
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognition-tool-gateway ./cmd/kognition-tool-gateway
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognition-model-gateway ./cmd/kognition-model-gateway
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognition-agent-communication-gateway ./cmd/kognition-agent-communication-gateway
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognition-intent-gateway ./cmd/kognition-intent-gateway
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognition-knowledge-gateway ./cmd/kognition-knowledge-gateway
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognition-knowledge-azuresearch ./cmd/kognition-knowledge-azuresearch
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognition-knowledge-pgvector ./cmd/kognition-knowledge-pgvector
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognition-migrate ./cmd/kognition-migrate
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -o /bin/kognition-workload-identity-proxy ./cmd/kognition-workload-identity-proxy

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99 AS runtime
COPY --from=builder /kognition.dependencies /usr/share/kognition/dependencies.txt
COPY LICENSE.TXT NOTICE PRIVACY /usr/share/kognition/
COPY --from=builder /usr/local/go/LICENSE /usr/local/go/PATENTS /usr/share/kognition/go/

FROM runtime AS migrate
USER 65532:65532
COPY --from=builder /bin/kognition-migrate /kognition-migrate
HEALTHCHECK NONE
ENTRYPOINT ["/kognition-migrate"]

# --- API Server ---
FROM runtime AS apiserver
USER 65532:65532
COPY --from=builder /bin/kognition-apiserver /kognition-apiserver
EXPOSE 8080 8081
HEALTHCHECK NONE
ENTRYPOINT ["/kognition-apiserver"]

# --- Controller Manager ---
FROM runtime AS controller
USER 65532:65532
COPY --from=builder /bin/kognition-controller-manager /kognition-controller-manager
HEALTHCHECK NONE
ENTRYPOINT ["/kognition-controller-manager"]

# --- CLI ---
FROM runtime AS ctl
USER 65532:65532
COPY --from=builder /bin/kognitionctl /kognitionctl
HEALTHCHECK NONE
ENTRYPOINT ["/kognitionctl"]

# --- K8s Controller Manager ---
FROM runtime AS k8s-controller
USER 65532:65532
COPY --from=builder /bin/kognition-k8s-controller /kognition-k8s-controller
HEALTHCHECK NONE
ENTRYPOINT ["/kognition-k8s-controller"]

# --- Memory Service ---
FROM runtime AS memory-service
USER 65532:65532
COPY --from=builder /bin/kognition-memory-service /kognition-memory-service
HEALTHCHECK NONE
ENTRYPOINT ["/kognition-memory-service"]

# --- Memory Policy Gateway ---
FROM runtime AS memory-policy-gateway
USER 65532:65532
COPY --from=builder /bin/kognition-memory-policy-gateway /kognition-memory-policy-gateway
EXPOSE 8091
HEALTHCHECK NONE
ENTRYPOINT ["/kognition-memory-policy-gateway"]

# --- Tool Gateway ---
FROM runtime AS tool-gateway
USER 65532:65532
COPY --from=builder /bin/kognition-tool-gateway /kognition-tool-gateway
EXPOSE 8092
HEALTHCHECK NONE
ENTRYPOINT ["/kognition-tool-gateway"]

# --- Model Gateway ---
FROM runtime AS model-gateway
USER 65532:65532
COPY --from=builder /bin/kognition-model-gateway /kognition-model-gateway
EXPOSE 8093
HEALTHCHECK NONE
ENTRYPOINT ["/kognition-model-gateway"]

# --- Agent Communication Gateway ---
FROM runtime AS agent-communication-gateway
USER 65532:65532
COPY --from=builder /bin/kognition-agent-communication-gateway /kognition-agent-communication-gateway
EXPOSE 8094
HEALTHCHECK NONE
ENTRYPOINT ["/kognition-agent-communication-gateway"]

# --- Kubernetes IntentGateway HTTP server ---
FROM runtime AS intent-gateway
USER 65532:65532
COPY --from=builder /bin/kognition-intent-gateway /kognition-intent-gateway
EXPOSE 8080 8081
HEALTHCHECK NONE
ENTRYPOINT ["/kognition-intent-gateway"]

# --- Knowledge Gateway ---
FROM runtime AS knowledge-gateway
USER 65532:65532
COPY --from=builder /bin/kognition-knowledge-gateway /kognition-knowledge-gateway
EXPOSE 8095
HEALTHCHECK NONE
ENTRYPOINT ["/kognition-knowledge-gateway"]

# --- Azure AI Search Knowledge Provider ---
FROM runtime AS knowledge-azuresearch
USER 65532:65532
COPY --from=builder /bin/kognition-knowledge-azuresearch /kognition-knowledge-azuresearch
EXPOSE 9443 9092
HEALTHCHECK NONE
ENTRYPOINT ["/kognition-knowledge-azuresearch"]

FROM runtime AS knowledge-pgvector
USER 65532:65532
COPY --from=builder /bin/kognition-knowledge-pgvector /kognition-knowledge-pgvector
EXPOSE 9443 9092
HEALTHCHECK NONE
ENTRYPOINT ["/kognition-knowledge-pgvector"]

FROM runtime AS workload-identity-proxy
USER 65532:65532
COPY --from=builder /bin/kognition-workload-identity-proxy /kognition-workload-identity-proxy
EXPOSE 8096
HEALTHCHECK NONE
ENTRYPOINT ["/kognition-workload-identity-proxy"]

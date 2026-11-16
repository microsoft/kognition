# syntax=docker/dockerfile:1
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.


# kognition-kb-ingester
#
# Runs as a Kubernetes Job launched by the KnowledgeBase reconciler. It:
#   1. Reads its full task spec from the KOGNITION_INGEST_INPUT env var (JSON).
#   2. For each source, fetches the raw bytes (inline, url, azure blob, s3),
#      parses to text (plain / markdown / pdf), splits into chunks per
#      chunkStrategy, and embeds via the KB's embeddingModelRef.
#   3. UPSERTs the (kb, source_uri, chunk_idx, content, embedding, metadata)
#      rows into the pgvector table configured on the KnowledgeBaseClass.
#   4. Prints a single-line JSON summary on stdout so the controller can
#      read stats and set them on the KB's status subresource.
#
# All Python code lives in this directory. Runtime image tag:
#   kognition/kb-ingester
FROM --platform=$BUILDPLATFORM golang:1.26.11-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS transport
ARG TARGETOS
ARG TARGETARCH
ENV GOOS=$TARGETOS GOARCH=$TARGETARCH

RUN apk add --no-cache git ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN --mount=type=cache,id=kognition-go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -trimpath -o /out/kognition-knowledge-indexer ./cmd/kognition-knowledge-indexer

FROM python:3.12-slim@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1 AS runtime

WORKDIR /app

# Native deps: libpq for psycopg (binary wheel bundles it, but keep gcc for
# any transitive source builds), curl for diagnostic use inside the Job, and
# git for the `git` KnowledgeBase source type.
RUN sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources \
    && apt-get update --error-on=any \
    && apt-get upgrade -y --no-install-recommends \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        libpq5 \
    && rm -rf /var/lib/apt/lists/*

COPY runtime/ingester/requirements.txt .
ARG PIP_INDEX_URL=https://pypi.org/simple
RUN case "$PIP_INDEX_URL" in https://*) ;; *) echo "package feed must use HTTPS" >&2; exit 1 ;; esac && \
	pip install --no-cache-dir -r requirements.txt && \
	python -m pip uninstall --yes pip

COPY runtime/ingester/*.py ./
COPY --from=transport /out/kognition-knowledge-indexer /app/kognition-knowledge-indexer
COPY LICENSE.TXT NOTICE PRIVACY /usr/share/kognition/
COPY --from=transport /usr/local/go/LICENSE /usr/local/go/PATENTS /usr/share/kognition/go/
COPY tools/distribution_notices.py /tmp/distribution_notices.py
ARG KOGNITION_NOTICE_STRICT=false
RUN python /tmp/distribution_notices.py python --include-interpreter --output /usr/share/kognition/python --strict "$KOGNITION_NOTICE_STRICT" \
    && rm /tmp/distribution_notices.py

RUN adduser --disabled-password --gecos "" ingester
USER ingester

ENV PYTHONUNBUFFERED=1

HEALTHCHECK NONE

ENTRYPOINT ["python", "ingest.py"]

# syntax=docker/dockerfile:1
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build
ENV CGO_ENABLED=0 GOMAXPROCS=2
ADD --checksum=sha256:e1e670e39f186360fa4243b18ca78a6b9c9d1ed46980054c3a4f32702bd42359 https://api.github.com/repos/seaweedfs/seaweedfs/tarball/b65832bdc9bbbcf3682465b1be598627ac386206 /source.tar.gz
RUN mkdir /source && tar -xzf /source.tar.gz -C /source --strip-components=1
WORKDIR /source
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    go list -mod=readonly -deps ./weed > /weed.dependencies && \
    if grep -E '^golang.org/x/crypto/openpgp(/|$)' /weed.dependencies; then exit 1; fi && \
    go build -mod=readonly -trimpath -ldflags="-s -w" -o /weed ./weed

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/seaweedfs/seaweedfs" \
      org.opencontainers.image.revision="b65832bdc9bbbcf3682465b1be598627ac386206" \
      org.opencontainers.image.version="4.49" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /weed /weed
COPY --from=build /source/LICENSE /usr/share/licenses/seaweedfs/LICENSE
COPY --from=build /weed.dependencies /usr/share/seaweedfs/dependencies.txt
USER 1000:1000
ENV HOME=/tmp
EXPOSE 8333
ENTRYPOINT ["/weed"]
CMD ["mini", "-dir=/data", "-admin.ui=false", "-s3.port.iceberg=0", "-s3.port.lance=0", "-master.volumeSizeLimitMB=16", "-volume.max=4"]

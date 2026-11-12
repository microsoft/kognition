# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM golang:1.26.6-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=bridge-source main.go ./samples/kagent-observability/bridge/main.go
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/bridge ./samples/kagent-observability/bridge

FROM kindest/node@sha256:cd224d8da58d50907d1dd41d476587643dad2ffd9f6a4d96caf530fb3b9a5956 AS kubectl

FROM scratch
COPY --from=build /out/bridge /bridge
COPY --from=kubectl /usr/bin/kubectl /usr/bin/kubectl
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/bridge"]
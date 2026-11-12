# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM golang:1.26.10-alpine@sha256:9ce231b28998b32e116927c61aa5c406183061d4f3f2d61b292c88e7f6bdf7ca AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /provider ./e2e/fixtures/sg08-provider
FROM scratch
COPY --from=build /provider /provider
USER 1000:1000
ENTRYPOINT ["/provider"]
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM golang:1.26.11-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /provider ./e2e/fixtures/sg08-provider
FROM scratch
COPY --from=build /provider /provider
USER 1000:1000
ENTRYPOINT ["/provider"]
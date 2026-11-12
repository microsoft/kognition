# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM golang:1.26.6-alpine AS build
WORKDIR /src
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /fixture main.go

FROM scratch
COPY --from=build /fixture /fixture
USER 65532:65532
EXPOSE 443
ENTRYPOINT ["/fixture"]
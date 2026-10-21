# syntax=docker/dockerfile:1

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /nuget-proxy ./cmd/nuget-proxy

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /nuget-proxy /nuget-proxy
COPY config.yaml /config/config.yaml
COPY LICENSE.md /LICENSE.md
# BuildKit creates WORKDIR with the current USER's ownership.
USER 65532:65532
WORKDIR /data
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/nuget-proxy"]
CMD ["-config", "/config/config.yaml"]

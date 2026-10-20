FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /nuget-proxy ./cmd/nuget-proxy \
    && mkdir /data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /nuget-proxy /nuget-proxy
COPY --from=build --chown=65532:65532 /data /data
COPY LICENSE.md /LICENSE.md
USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/nuget-proxy"]
CMD ["-config", "/config/config.yaml"]

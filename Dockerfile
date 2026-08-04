FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY cli ./cli
COPY domain ./domain
COPY infra ./infra
COPY adapter ./adapter
COPY api ./api
COPY sdk ./sdk
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/filegate ./cmd/filegate

# Distroless has no shell, so the runtime directories are staged in the build
# image and copied in with the right ownership. Without them a container that
# configures nothing cannot create its default mount and refuses to start.
RUN mkdir -p /stage/var/lib/filegate/data /stage/var/lib/filegate/index /stage/var/lib/filegate/config \
    && chown -R 65532:65532 /stage/var/lib/filegate

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build --chown=65532:65532 /stage/var/lib/filegate /var/lib/filegate
COPY --from=build /out/filegate /app/filegate
EXPOSE 8080/tcp
ENTRYPOINT ["/app/filegate"]
CMD ["serve"]

FROM golang:1.25 AS build
ARG VERSION=development
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY cli ./cli
COPY domain ./domain
COPY infra ./infra
COPY adapter ./adapter
COPY api ./api
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/k2b-dev/filegate/v5/cli.Version=${VERSION}" -o /out/filegate ./cmd/filegate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/filegate /app/filegate
EXPOSE 8080
ENTRYPOINT ["/app/filegate"]
CMD ["serve", "--config", "/etc/filegate/conf.yaml"]

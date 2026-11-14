# QuickBooks Sync module image. Build from the repository root: docker build .
FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/quickbooks ./cmd/quickbooks

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/quickbooks /quickbooks
EXPOSE 8080
ENTRYPOINT ["/quickbooks", "serve"]

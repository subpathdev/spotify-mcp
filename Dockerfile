FROM golang:1.27.4-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /spotify-mcp ./cmd/spotify-mcp

# A static binary plus the CA bundle for api.spotify.com and accounts.spotify.com.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /spotify-mcp /spotify-mcp
USER 65532:65532
ENV DATA_DIR=/data
ENTRYPOINT ["/spotify-mcp"]

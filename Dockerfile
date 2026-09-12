FROM golang:1.23-bookworm AS build
WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ircapybara ./cmd/ircapybara
RUN mkdir -p /out/data/tls

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/ircapybara /ircapybara
COPY --from=build --chown=65532:65532 /out/data /data

EXPOSE 6697/tcp
ENTRYPOINT ["/ircapybara"]

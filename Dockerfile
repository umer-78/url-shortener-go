FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=0 with the pure-Go SQLite driver gives a static binary
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/shortener ./cmd/shortener

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/shortener /shortener
USER 65532:65532
ENV DB=/data/links.db ADDR=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/shortener"]

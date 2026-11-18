FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /shadowbox ./cmd/shadowbox

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /shadowbox /shadowbox
EXPOSE 8080 9000
ENTRYPOINT ["/shadowbox"]
CMD ["-config", "/etc/shadowbox/shadowbox.yaml"]

FROM golang:1.27-alpine AS b
WORKDIR /s
COPY main.go .
RUN go mod init probe && go get github.com/envoyproxy/go-control-plane/envoy@latest google.golang.org/grpc@latest && go mod tidy && CGO_ENABLED=0 go build -o /p . && cp go.mod go.sum /
FROM alpine:3.22
COPY --from=b /p /p
COPY --from=b /go.mod /go.sum /
ENTRYPOINT ["/p"]

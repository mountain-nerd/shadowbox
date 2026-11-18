FROM golang:1.27-alpine AS build
WORKDIR /src
COPY main.go .
RUN go mod init prompt-swap \
 && go get github.com/envoyproxy/go-control-plane/envoy@latest google.golang.org/grpc@latest \
 && go mod tidy \
 && CGO_ENABLED=0 go build -o /prompt-swap .

FROM alpine:3.22
COPY --from=build /prompt-swap /prompt-swap
ENTRYPOINT ["/prompt-swap"]

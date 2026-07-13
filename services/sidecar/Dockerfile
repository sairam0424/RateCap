FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.work go.work.sum ./
COPY proto/ proto/
COPY services/sidecar/ services/sidecar/
WORKDIR /src/services/sidecar
RUN GOWORK=off go build -o /ratecap-sidecar .

FROM alpine:3.20
COPY --from=build /ratecap-sidecar /usr/local/bin/ratecap-sidecar
ENTRYPOINT ["/usr/local/bin/ratecap-sidecar"]

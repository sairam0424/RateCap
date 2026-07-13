FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.work go.work.sum ./
COPY proto/ proto/
COPY services/core/ services/core/
WORKDIR /src/services/core
RUN GOWORK=off go build -o /ratecap-core .

FROM alpine:3.20
COPY --from=build /ratecap-core /usr/local/bin/ratecap-core
ENTRYPOINT ["/usr/local/bin/ratecap-core"]

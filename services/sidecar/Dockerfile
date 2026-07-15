FROM golang:1.26-alpine AS build
WORKDIR /src
COPY proto/ proto/
COPY services/core/ services/core/
COPY services/sidecar/ services/sidecar/
RUN printf 'go 1.26.2\n\nuse ./proto\nuse ./services/core\nuse ./services/sidecar\n' > go.work
WORKDIR /src/services/sidecar
RUN go build -o /ratecap-sidecar .

FROM alpine:3.20
COPY --from=build /ratecap-sidecar /usr/local/bin/ratecap-sidecar
ENTRYPOINT ["/usr/local/bin/ratecap-sidecar"]

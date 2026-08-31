FROM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build
WORKDIR /src
COPY proto/ proto/
COPY services/core/ services/core/
COPY services/sidecar/ services/sidecar/
RUN printf 'go 1.26.2\n\nuse ./proto\nuse ./services/core\nuse ./services/sidecar\n' > go.work
WORKDIR /src/services/sidecar
RUN go build -o /ratecap-sidecar .

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
COPY --from=build /ratecap-sidecar /usr/local/bin/ratecap-sidecar
ENTRYPOINT ["/usr/local/bin/ratecap-sidecar"]

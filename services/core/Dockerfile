FROM golang:1.27-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS build
WORKDIR /src
COPY proto/ proto/
COPY services/core/ services/core/
RUN printf 'go 1.26.2\n\nuse ./proto\nuse ./services/core\n' > go.work
WORKDIR /src/services/core
RUN go build -o /ratecap-core .

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
COPY --from=build /ratecap-core /usr/local/bin/ratecap-core
ENTRYPOINT ["/usr/local/bin/ratecap-core"]

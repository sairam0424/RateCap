FROM golang:1.26-alpine AS build
WORKDIR /src
COPY proto/ proto/
COPY services/core/ services/core/
RUN printf 'go 1.26.2\n\nuse ./proto\nuse ./services/core\n' > go.work
WORKDIR /src/services/core
RUN go build -o /ratecap-core .

FROM alpine:3.20
COPY --from=build /ratecap-core /usr/local/bin/ratecap-core
ENTRYPOINT ["/usr/local/bin/ratecap-core"]

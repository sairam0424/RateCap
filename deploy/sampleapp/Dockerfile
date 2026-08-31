FROM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build
WORKDIR /src
COPY packages/ packages/
COPY deploy/sampleapp/ deploy/sampleapp/
RUN printf 'go 1.26.2\n\nuse ./packages/sdks/go\nuse ./deploy/sampleapp\n' > go.work
WORKDIR /src/deploy/sampleapp
RUN go build -o /sampleapp .

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
COPY --from=build /sampleapp /usr/local/bin/sampleapp
ENTRYPOINT ["/usr/local/bin/sampleapp"]

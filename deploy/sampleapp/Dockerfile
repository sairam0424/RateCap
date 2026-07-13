FROM golang:1.26-alpine AS build
WORKDIR /src
COPY packages/ packages/
COPY deploy/sampleapp/ deploy/sampleapp/
RUN printf 'go 1.26.2\n\nuse ./packages/sdks/go\nuse ./deploy/sampleapp\n' > go.work
WORKDIR /src/deploy/sampleapp
RUN go build -o /sampleapp .

FROM alpine:3.20
COPY --from=build /sampleapp /usr/local/bin/sampleapp
ENTRYPOINT ["/usr/local/bin/sampleapp"]

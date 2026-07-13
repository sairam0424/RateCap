# deploy/sampleapp/Dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.work go.work.sum ./
COPY packages/ packages/
COPY deploy/sampleapp/ deploy/sampleapp/
WORKDIR /src/deploy/sampleapp
RUN GOWORK=off go build -o /sampleapp .

FROM alpine:3.20
COPY --from=build /sampleapp /usr/local/bin/sampleapp
ENTRYPOINT ["/usr/local/bin/sampleapp"]

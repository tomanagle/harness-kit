FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY server/go.mod server/go.sum ./server/
COPY packages/client-go/go.mod ./packages/client-go/go.mod
RUN cd server && go mod download
COPY server ./server
RUN cd server && CGO_ENABLED=0 go build -trimpath -o /server ./cmd
FROM alpine:3.22
RUN apk add --no-cache ca-certificates && addgroup -g 10001 harness && adduser -D -u 10001 -G harness harness
USER harness
COPY --from=build /server /usr/local/bin/harness-kit-server
EXPOSE 8080
ENTRYPOINT ["harness-kit-server"]

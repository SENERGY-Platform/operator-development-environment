FROM golang:1.26 AS builder

COPY . /go/src/app
WORKDIR /go/src/app

ENV GO111MODULE=on
ENV CGO_ENABLED=0

# Writes docs/, which pkg/api imports and serves at /doc. Not committed, so this
# has to run before the build rather than after it.
RUN go generate ./...
RUN go build -o ode .

FROM alpine:3.22

RUN apk --no-cache add ca-certificates

WORKDIR /root/
COPY --from=builder /go/src/app/ode .
COPY --from=builder /go/src/app/config.json .

EXPOSE 8080

CMD ["./ode"]

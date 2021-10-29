FROM golang:1.17.2
WORKDIR /usr/src/alerter

build:
  COPY . .
  RUN CGO_ENABLED=0 GOOS=linux go build -mod=vendor -ldflags="-s -w" \
    -o query_alert \
    ./example/query_alert/main.go
  SAVE ARTIFACT query_alert AS LOCAL build/bin/query_alert

docker:
  #FROM scratch
  FROM alpine:3.13.6
  WORKDIR /
  COPY +build/query_alert /query_alert
  #COPY +dockerfiles/etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
  ENV USER root
  ENTRYPOINT ["/query_alert"]
  SAVE IMAGE mxpaul/query_alert:v1.0.0


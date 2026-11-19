FROM golang:1.26.11-alpine3.24@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS build
RUN apk add --no-cache gcc musl-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd/ cmd/
COPY internal/ internal/
ARG VERSION=dev
RUN CGO_ENABLED=1 go build -mod=readonly -trimpath -ldflags="-s -w -X=main.version=${VERSION}" -o /emailrelay ./cmd/emailrelay

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 relay \
    && adduser -S -D -H -u 10001 -G relay -h /data relay \
    && mkdir -p /data/emailrelay \
    && chown -R relay:relay /data \
    && chmod 0700 /data /data/emailrelay
ENV HOME=/data RELAY_DATA=/data/emailrelay
COPY --from=build /emailrelay /usr/local/bin/emailrelay
COPY deploy/licenses/ /usr/share/licenses/emailrelay/
COPY LICENSE /usr/share/licenses/emailrelay/LICENSE
USER 10001:10001
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/emailrelay"]

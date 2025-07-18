FROM alpine:latest	AS builder

RUN apk add ca-certificates && update-ca-certificates

FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs
COPY --from=bin telegraf /usr/bin/telegraf

ENTRYPOINT ["telegraf"]

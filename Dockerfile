FROM golang:1.23 AS builder
COPY server/main.go .
RUN CGO_ENABLED=0 go build -o /server main.go

FROM busybox AS runner
COPY --from=builder /server /server
ENTRYPOINT [ "/server" ]

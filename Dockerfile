FROM golang:1.27 AS builder
ENV CGO_ENABLED=0
WORKDIR /go/src/app
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /filestor

FROM alpine:3.22
RUN apk add --no-cache tini ca-certificates
COPY --from=builder /filestor /filestor
ENV FILESTOR_CONFIG=/config.yaml
EXPOSE 8080
ENTRYPOINT ["tini", "--"]
CMD ["/filestor"]

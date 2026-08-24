FROM golang:1.27 AS builder
ENV CGO_ENABLED=0
WORKDIR /go/src/app
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /filestor

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
	tini \
	ca-certificates \
	libreoffice-writer \
	libreoffice-calc \
	libreoffice-impress \
	imagemagick \
	libmagickcore-6.q16-6-extra \
	libheif1 \
	libwebp7 \
	poppler-utils \
	pandoc \
	catdoc \
	fonts-noto-cjk \
	&& rm -rf /var/lib/apt/lists/*
COPY --from=builder /filestor /filestor
ENV FILESTOR_CONFIG=/config.yaml
ENV HOME=/tmp
ENV SAL_USE_VCLPLUGIN=svp
EXPOSE 8080
ENTRYPOINT ["tini", "--"]
CMD ["/filestor"]

FROM oven/bun:1 AS static
# Mirror the repo layout: main.css's tailwind @source glob ("../../../*.go")
# scans the top-level Go files for fileIcon's icon class names, so the Go
# files must sit next to web/ — resolving past them (to /) walks the whole
# container filesystem and the build hangs.
WORKDIR /repo/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
COPY *.go /repo/
RUN bun run build

FROM golang:1.27 AS builder
ENV CGO_ENABLED=0
WORKDIR /go/src/app
COPY . .
COPY --from=static /repo/web/dist web/dist
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
	ffmpeg \
	fonts-noto-cjk \
	&& rm -rf /var/lib/apt/lists/*
COPY --from=builder /filestor /filestor
ENV FILESTOR_CONFIG=/config.yaml
ENV HOME=/tmp
ENV SAL_USE_VCLPLUGIN=svp
EXPOSE 8080
ENTRYPOINT ["tini", "--"]
CMD ["/filestor"]

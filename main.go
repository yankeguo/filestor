package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yankeguo/rg"
)

func main() {
	var err error
	defer func() {
		if err == nil {
			return
		}
		log.Println("exited with error:", err)
		os.Exit(1)
	}()
	defer rg.Guard(&err)

	configPath := envOr("FILESTOR_CONFIG", "config.yaml")
	listen := envOr("FILESTOR_LISTEN", ":8080")
	flag.StringVar(&configPath, "config", configPath, "path to yaml config")
	flag.StringVar(&listen, "listen", listen, "http listen address")
	flag.Parse()

	cfg := rg.Must(loadConfig(configPath))
	log.Printf("loaded config from %s", configPath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              listen,
		Handler:           NewServer(cfg, rg.Must(newOSSStore(cfg.Aliyun.OSS))).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Println("listening on", listen)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err = <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		return
	case <-ctx.Done():
		log.Println("shutting down")
	}

	stop()
	err = srv.Shutdown(context.Background())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

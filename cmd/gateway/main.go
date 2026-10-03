// Command gateway is a rate-limiting reverse proxy.
//
//	gateway -config config.yaml
//	gateway -check -config config.yaml              # validate only
//	gateway -probe http://127.0.0.1:9090/healthz   # container health check (distroless has no curl)
//	gateway -version
//
// SIGHUP reloads routes and limits; SIGINT/SIGTERM shut down gracefully.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/srujanmalakpata/floodgate/internal/app"
	"github.com/srujanmalakpata/floodgate/internal/config"
)

// version is set at build time: go build -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", envOr("RLGW_CONFIG", "config.yaml"), "path to YAML or JSON config (env RLGW_CONFIG)")
	probe := flag.String("probe", "", "GET this URL, exit 0 on HTTP 200 (for container health checks), then exit")
	check := flag.Bool("check", false, "validate the config file and exit (like nginx -t)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("gateway", version)
		return nil
	}
	if *probe != "" {
		return probeURL(*probe)
	}
	if *check {
		if _, _, err := config.Load(*cfgPath); err != nil {
			return err
		}
		fmt.Println("config ok:", *cfgPath)
		return nil
	}

	level := new(slog.LevelVar)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	a, err := app.New(app.Options{ConfigPath: *cfgPath, Logger: logger, LogLevel: level, Version: version})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := a.Config()
	var lc net.ListenConfig
	proxyLn, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return err
	}
	adminLn, err := lc.Listen(ctx, "tcp", cfg.AdminListen)
	if err != nil {
		_ = proxyLn.Close()
		return err
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				logger.Info("SIGHUP received, reloading config")
				_ = a.Reload() // errors are logged and counted by the reloader
			}
		}
	}()

	return a.Serve(ctx, proxyLn, adminLn)
}

func probeURL(u string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("probe %s: status %d", u, res.StatusCode)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

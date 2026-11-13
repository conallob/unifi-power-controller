// Command unifi-power-controller serves PoE power control for lab nodes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ubiquiti-community/go-unifi/unifi"

	"github.com/conallob/unifi-power-controller/internal/config"
	"github.com/conallob/unifi-power-controller/internal/metrics"
	"github.com/conallob/unifi-power-controller/internal/power"
	"github.com/conallob/unifi-power-controller/internal/server"
	"github.com/conallob/unifi-power-controller/internal/version"
)

func main() {
	cfgPath := flag.String("config", "/etc/unifi-power-controller/config.yaml", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	v, c, d := version.Fields()
	log.Info("starting", "version", v, "commit", c, "built", d)
	if err := run(*cfgPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	timeout := int(cfg.UniFi.Timeout.Seconds())
	client, err := unifi.New(ctx, &unifi.Config{
		BaseURL:        cfg.UniFi.URL,
		APIKey:         cfg.UniFi.APIKey,
		AllowInsecure:  cfg.UniFi.InsecureSkipVerify,
		TimeoutSeconds: &timeout,
	})
	if err != nil {
		return err
	}

	v, c, d := version.Fields()
	srv := &http.Server{
		Addr: cfg.Listen,
		Handler: server.New(server.Options{
			Power:         power.New(client, cfg.UniFi.Site, cfg.PowerTargets(), power.WithMinOffTime(*cfg.MinOffTime)),
			Logger:        log,
			SHA256Secrets: cfg.RPC.SHA256Secrets,
			SHA512Secrets: cfg.RPC.SHA512Secrets,
			SignedHeaders: cfg.RPC.SignedHeaders,
			Metrics:       metrics.Handler(metrics.NewRegistry(metrics.Info{Version: v, Commit: c, Date: d})),
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.Listen, "nodes", len(cfg.Nodes), "rpcAuth", len(cfg.RPC.SHA256Secrets)+len(cfg.RPC.SHA512Secrets) > 0)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

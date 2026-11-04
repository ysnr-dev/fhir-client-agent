// fhir-client-agent は院内に常駐し、院内システムの通知をカルテ(fhir-client)へ中継する。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ysnr-dev/fhir-client-agent/internal/adapter/orcapush"
	"github.com/ysnr-dev/fhir-client-agent/internal/adapter/ssmix2"
	"github.com/ysnr-dev/fhir-client-agent/internal/agent"
	"github.com/ysnr-dev/fhir-client-agent/internal/config"
	"github.com/ysnr-dev/fhir-client-agent/internal/deliver"
)

// version はリリースビルドで -ldflags "-X main.version=..." により埋める。
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(version)
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fhir-client-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	exeDir, err := config.ExecutableDir()
	if err != nil {
		exeDir = "."
	}
	cfg, err := config.Load(config.ExecutableDirEnv(os.Getenv), exeDir)
	if err != nil {
		return err
	}

	// journald でも docker logs でもそのまま検索できるよう、1 行 1 JSON で出す。
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	adapter, err := buildAdapter(cfg, log)
	if err != nil {
		return err
	}
	// 通知を届けるアダプタだけが送達を持つ。ssmix2-storage は取りに行って書くだけ。
	var deliverer *deliver.Deliverer
	if cfg.EventsURL != "" {
		deliverer, err = deliver.New(cfg.EventsURL, cfg.InboundToken, cfg.SpoolDir, log)
		if err != nil {
			return err
		}
	}

	// SIGTERM(systemd / docker stop)で、未送信のイベントを退避してから終わる。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := agent.New(adapter, deliverer, log)
	if cfg.HealthAddr != "" {
		if err := a.ServeHealth(ctx, cfg.HealthAddr); err != nil {
			return fmt.Errorf("HEALTH_ADDR: %w", err)
		}
	}

	log.Info("starting", "version", version, "adapter", adapter.Name(),
		"events_url", cfg.EventsURL, "spool_dir", cfg.SpoolDir, "ssmix2_url", cfg.Ssmix2.MessagesURL)
	a.Run(ctx)
	return nil
}

func buildAdapter(cfg config.Config, log *slog.Logger) (agent.Adapter, error) {
	switch cfg.Adapter {
	case config.AdapterOrcaPush:
		return orcapush.New(cfg.Orca, log)
	case config.AdapterSsmix2Storage:
		return ssmix2.New(cfg.Ssmix2, cfg.InboundToken, log), nil
	}
	return nil, fmt.Errorf("AGENT_ADAPTER=%q は対応していません", cfg.Adapter)
}

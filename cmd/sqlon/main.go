package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"sqlon/internal/app"
)

func main() {
	// SIGTERM (docker stop, systemd, Kubernetes) shuts down cleanly so the
	// early-warning history written every ten minutes is not cut short.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.DefaultRuntime().Run(ctx, os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aibrix/neocloud-node/pkg/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, os.Args[1:], os.Getenv); err != nil && !errors.Is(err, context.Canceled) {
		_, _ = fmt.Fprintf(os.Stderr, "neocloud-node: %v\n", err)
		os.Exit(1)
	}
}

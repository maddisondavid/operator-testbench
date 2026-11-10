package main

import (
	"context"
	"operator-dynamo/internal/app"
	"operator-dynamo/internal/cli"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx, os.Args[1:], os.Stderr, app.New())
	stop()
	os.Exit(code)
}

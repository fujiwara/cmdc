package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	app "github.com/fujiwara/cmdc"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := app.Run(ctx)
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*app.ExitError](err); ok {
		if ee.Err != nil {
			slog.Error(ee.Err.Error())
		}
		return ee.Code
	}
	slog.Error(err.Error())
	return 1
}

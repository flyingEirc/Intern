package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/flyingEirc/Intern/app/web/cmd"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cmd.Execute(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "intern:", err)
		os.Exit(1)
	}
}

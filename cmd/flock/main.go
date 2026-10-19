// Flock is Croc's file-transfer engine with an agent-first JSON CLI and native QUIC.
package main

import (
	"context"
	"github.com/schollz/croc/v11/internal/flockcli"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(flockcli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

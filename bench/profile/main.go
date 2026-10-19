// Profile runs the real Flock CLI while collecting a CPU profile for benchmarks.
package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/pprof"
	"syscall"

	"github.com/mhadifilms/flock/internal/flockcli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var profile *os.File
	if name := os.Getenv("FLOCK_CPU_PROFILE"); name != "" {
		file, err := os.Create(name)
		if err != nil {
			panic(err)
		}
		if err = pprof.StartCPUProfile(file); err != nil {
			panic(err)
		}
		profile = file
	}
	status := flockcli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	pprof.StopCPUProfile()
	if profile != nil {
		profile.Close()
	}
	os.Exit(status)
}

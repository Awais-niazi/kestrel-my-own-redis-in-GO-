// Command kestreld is the Kestrel server.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"kestrel/command"
	"kestrel/config"
	"kestrel/server"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "kestreld: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	for _, a := range args {
		switch a {
		case "-h", "--help":
			usage(os.Stdout)
			return nil
		case "--help-config":
			printConfigReference(os.Stdout)
			return nil
		case "-v", "--version":
			fmt.Printf("kestreld %s (RESP compatibility target %s)\n",
				command.Version, command.RESPCompat)
			return nil
		}
	}

	cfg, err := config.Load(args)
	if err != nil {
		return err
	}

	srv, err := server.New(cfg)
	if err != nil {
		return err
	}

	// SIGTERM and SIGINT start the graceful shutdown sequence: stop
	// accepting, drain in-flight commands, flush durable state (NFR-3).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv.Logger().Info("starting",
		"version", command.Version,
		"pid", os.Getpid(),
		"config", cfg.Snapshot().Source,
		"run_id", command.RunID())

	return srv.Serve(ctx)
}

// printConfigReference lists every parameter and its default, so that the
// binary is self-documenting rather than requiring the file from Appendix B.
func printConfigReference(w *os.File) {
	defaults := config.Default()
	for _, name := range config.Names() {
		v, _ := defaults.Get(name)
		if v == "" {
			v = `""`
		}
		fmt.Fprintf(w, "%-32s %s\n", name, v)
	}
}

func usage(w *os.File) {
	fmt.Fprintf(w, `kestreld %s - a RESP-compatible in-memory data store

Usage:
  kestreld [/path/to/kestrel.conf] [--option value ...]

Options are the directives from the configuration file, passed as flags:

  kestreld --port 6380 --maxmemory 256mb --appendfsync everysec

Configuration is resolved in this order, with later sources winning:
  defaults  <  config file  <  KESTREL_* environment variables  <  flags

Run 'kestreld --help-config' to list every parameter with its default.
`, command.Version)
}

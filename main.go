package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/parichit/gpm/cmd"
	"github.com/parichit/gpm/internal/daemon"
)

func main() {
	// Hidden subcommand used when daemonizing
	if len(os.Args) == 2 && os.Args[1] == "__daemon_run" {
		runDaemon()
		return
	}

	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func runDaemon() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot get home dir:", err)
		os.Exit(1)
	}
	baseDir := filepath.Join(home, ".gpm")
	os.MkdirAll(baseDir, 0755)

	// Write PID file
	pidFile := filepath.Join(baseDir, "daemon.pid")
	os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644)
	defer os.Remove(pidFile)

	d, err := daemon.New(baseDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon init error:", err)
		os.Exit(1)
	}

	if err := d.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "daemon error:", err)
		os.Exit(1)
	}
}

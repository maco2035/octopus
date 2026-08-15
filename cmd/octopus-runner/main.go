// Command octopus-runner is the small binary installed on each dev machine
// (PLAN.md Phase 7): it opens a persistent outbound connection to the
// central Octopus server, authenticates with a per-runner token, and
// executes whatever GitJobs the server dispatches to it — using the exact
// same git/CLI execution code (internal/tools) Phase 6 runs directly on
// the server.
//
// It also hosts a local Web Dashboard on port 8088 (configurable) allowing
// developers to inspect toolchain health, configure server connection settings,
// view active jobs, and monitor execution history.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"octopus/internal/runner"
)

func main() {
	configPath := os.Getenv("OCTOPUS_RUNNER_CONFIG")
	if configPath == "" {
		configPath = "runner.yaml"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	manager := runner.NewManager(configPath)
	webServer := runner.NewWebServer(manager, manager.GetWebPort())

	if err := webServer.Start(ctx); err != nil {
		log.Fatalf("runner web UI: %v", err)
	}

	webURL := fmt.Sprintf("http://localhost:%d", manager.GetWebPort())
	fmt.Printf("\n========================================================================\n")
	fmt.Printf("  🐙 Octopus Runner started!\n")
	fmt.Printf("  🌐 Local Web Dashboard: %s\n", webURL)
	fmt.Printf("  📁 Config file:         %s\n", configPath)
	fmt.Printf("========================================================================\n\n")

	manager.Start(ctx)

	<-ctx.Done()
	fmt.Println("\nShutting down runner...")
	manager.Stop()
}

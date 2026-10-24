package main

import (
	"context"
	"log"
	"os/exec"
	"strings"
	"time"
)

// runScript executes the refresh script once and logs the outcome.
func runScript(ctx context.Context, path string) {
	started := time.Now()
	command := exec.CommandContext(ctx, "sh", path)
	output, err := command.CombinedOutput()

	elapsed := time.Since(started).Round(time.Millisecond)
	trimmed := strings.TrimSpace(string(output))

	if err != nil {
		log.Printf("script %s failed after %s: %v", path, elapsed, err)
		if trimmed != "" {
			log.Printf("script output:\n%s", trimmed)
		}
		return
	}

	log.Printf("script %s ok in %s", path, elapsed)
	if trimmed != "" {
		log.Printf("script output:\n%s", trimmed)
	}
}

// runScriptLoop runs the script once immediately, then every interval until ctx
// is cancelled. Runs never overlap: a slow script delays the next tick instead
// of stacking up, and a failing script is logged and left running.
func runScriptLoop(ctx context.Context, path string, every time.Duration) {
	runScript(ctx, path)

	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runScript(ctx, path)
		}
	}
}

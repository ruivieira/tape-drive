package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeScript(t *testing.T, dir, body string) string {
	t.Helper()

	path := filepath.Join(dir, "refresh.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// waitFor polls until want bytes have been appended to path.
func waitFor(t *testing.T, path string, want int, within time.Duration) int {
	t.Helper()

	deadline := time.Now().Add(within)
	got := 0
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			got = len(data)
			if got >= want {
				return got
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return got
}

func TestRunScriptLoopRunsImmediatelyAndPeriodically(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	script := writeScript(t, dir, "printf x >> '"+counter+"'\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runScriptLoop(ctx, script, 30*time.Millisecond)

	// One run happens before the first tick, so three runs prove the loop.
	if got := waitFor(t, counter, 3, 2*time.Second); got < 3 {
		t.Fatalf("script ran %d times, want at least 3", got)
	}
}

func TestRunScriptLoopSurvivesFailures(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	script := writeScript(t, dir, "printf x >> '"+counter+"'\nexit 1\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runScriptLoop(ctx, script, 30*time.Millisecond)

	if got := waitFor(t, counter, 2, 2*time.Second); got < 2 {
		t.Fatalf("failing script ran %d times, want at least 2", got)
	}
}

func TestRunScriptLoopStopsOnCancel(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	script := writeScript(t, dir, "printf x >> '"+counter+"'\n")

	ctx, cancel := context.WithCancel(context.Background())
	go runScriptLoop(ctx, script, 20*time.Millisecond)

	if got := waitFor(t, counter, 2, 2*time.Second); got < 2 {
		t.Fatalf("script ran %d times, want at least 2", got)
	}

	cancel()
	time.Sleep(50 * time.Millisecond)
	stopped := len(mustRead(t, counter))

	time.Sleep(150 * time.Millisecond)
	if grew := len(mustRead(t, counter)); grew != stopped {
		t.Fatalf("script kept running after cancel: %d -> %d", stopped, grew)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

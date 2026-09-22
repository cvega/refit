package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"refit/internal/migrate"
)

func TestHelpAndOfflineGuards(t *testing.T) {
	for _, command := range [][]string{{"help"}, {"prepare", "-help"}} {
		var output bytes.Buffer
		_ = run(context.Background(), command, &output, &output)
		if output.Len() == 0 {
			t.Fatal("missing help")
		}
		if command[0] == "help" && !strings.HasPrefix(output.String(), "refit COMMAND [flags]") {
			t.Fatal("help does not identify the refit command")
		}
	}
	t.Setenv("GH_PAT", "")
	t.Setenv("GH_SOURCE_PAT", "")
	for _, command := range [][]string{{"unknown"}, {"export"}, {"prepare"}, {"stage"}, {"lfs-push"}, {"verify"}, {"status"}} {
		var output bytes.Buffer
		if err := run(context.Background(), command, &output, &output); err == nil {
			t.Fatalf("accepted incomplete %v", command)
		}
	}
	var output bytes.Buffer
	_ = run(context.Background(), []string{"prepare", "-help"}, &output, &output)
	if !strings.Contains(output.String(), "1000000000") {
		t.Fatal("wrong default feature-flag threshold")
	}
}

func TestWaitForImport(t *testing.T) {
	for _, terminal := range []string{"SUCCEEDED", "FAILED", "FAILED_VALIDATION", "CANCELED", "CANCELLED", "UNKNOWN"} {
		t.Run(terminal, func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			calls := 0
			pending := []string{"NOT_STARTED", "QUEUED", "PENDING", "PENDING_VALIDATION", "VALIDATING", "WAITING", "IN_PROGRESS"}
			err := waitForImport(context.Background(), func(context.Context) (string, string, error) {
				calls++
				if calls <= len(pending) {
					return pending[calls-1], "", nil
				}
				return terminal, "", nil
			}, &output, &diagnostics, time.Millisecond, time.Second)
			if (err == nil) != (terminal == "SUCCEEDED") || calls != len(pending)+1 {
				t.Fatalf("calls=%d, error=%v", calls, err)
			}
			if terminal == "FAILED_VALIDATION" && (!strings.Contains(output.String(), terminal) || !strings.Contains(err.Error(), "did not succeed")) {
				t.Fatal("validation failure was not reported as a terminal result")
			}
			if !strings.Contains(diagnostics.String(), "IN_PROGRESS") || !strings.Contains(diagnostics.String(), terminal) {
				t.Fatal("missing progress")
			}
		})
	}
}

func TestWaitForImportStops(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout", "api-error"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var output bytes.Buffer
			calls := 0
			err := waitForImport(ctx, func(context.Context) (string, string, error) {
				calls++
				if mode == "cancel" {
					cancel()
				}
				if mode == "api-error" {
					return "", "", errors.New("unavailable")
				}
				return "IN_PROGRESS", "", nil
			}, &output, &output, time.Hour, 20*time.Millisecond)
			if err == nil || calls != 1 {
				t.Fatalf("calls=%d, error=%v", calls, err)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) || mode == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("wrong cancellation error: %v", err)
			}
		})
	}
}

func TestStatusWaitGuards(t *testing.T) {
	t.Setenv("GH_PAT", "")
	work := t.TempDir()
	if err := migrate.WriteJSON(filepath.Join(work, "staging.json"), migrate.Staging{MigrationID: "RM_test", TargetAPI: "https://api.github.com", Organization: "example", Repository: "trial-staging"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"status", "-work", work, "-migration-id", "other"},
		{"status", "-work", work, "-target-api", "https://other.invalid"},
		{"status", "-work", work, "-wait"},
		{"status", "-wait", "-poll-interval", "0s"},
		{"stage", "-wait", "-wait-timeout", "0s"},
		{"export", "-wait"},
	} {
		var output bytes.Buffer
		if err := run(context.Background(), args, &output, &output); err == nil {
			t.Fatalf("accepted invalid invocation: %v", args)
		}
	}
}

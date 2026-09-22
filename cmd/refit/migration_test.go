package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"refit/internal/migrate"
)

func TestMigrationPipelineResume(t *testing.T) {
	for _, interruption := range []string{"export-status", "download", "prepare", "status", "lfs-push", "stage", "policy", "approval"} {
		t.Run(interruption, func(t *testing.T) {
			work := t.TempDir()
			policy := filepath.Join(work, "policy.json")
			if err := migrate.WriteJSON(policy, migrate.ArchivePolicy{Git: migrate.MetadataPolicy{Reviewed: true}, Metadata: migrate.MetadataPolicy{Reviewed: true}}); err != nil {
				t.Fatal(err)
			}
			options := migrationOptions{Work: work, SourceURL: "https://github.com/source/repo", TargetOrg: "target", StagingRepo: "repo-staging", Policy: policy, LFSObjects: work, Confirm: true, RouteReviewed: true, PollInterval: time.Millisecond, WaitTimeout: time.Second}
			if interruption == "policy" {
				options.Policy = ""
			}
			if interruption == "approval" {
				options.Confirm = false
			}
			calls := make(map[string]int)
			command := func(ctx context.Context, args []string, output, diagnostics io.Writer) error {
				calls[args[0]]++
				value := func(flag string) string {
					for index, arg := range args {
						if arg == flag && index+1 < len(args) {
							return args[index+1]
						}
					}
					return ""
				}
				if args[0] == interruption && calls[args[0]] == 1 {
					return errors.New("interrupted")
				}
				switch args[0] {
				case "export":
					return json.NewEncoder(output).Encode(map[string]any{"export_id": calls["export"], "kind": value("-kind")})
				case "export-status":
					fmt.Fprint(output, `{"state":"exported"}`)
				case "download":
					return os.WriteFile(value("-out"), []byte("immutable archive"), 0600)
				case "prepare":
					return os.MkdirAll(value("-work"), 0700)
				case "stage":
					return migrate.WriteJSON(filepath.Join(value("-work"), "staging.json"), migrate.Staging{MigrationID: "known-migration", Organization: options.TargetOrg, Repository: options.StagingRepo, TargetAPI: options.TargetAPI})
				case "inspect", "verify", "status", "lfs-push":
				default:
					t.Fatalf("unexpected command %s", args[0])
				}
				return nil
			}
			fetch := func(context.Context, string, string, string) error {
				t.Fatal("fetched despite supplied cache")
				return nil
			}
			if err := runMigrationPipeline(context.Background(), options, command, fetch, io.Discard, io.Discard); err == nil {
				t.Fatal("expected interruption")
			}
			if interruption == "policy" || interruption == "approval" {
				if calls["stage"] != 0 {
					t.Fatal("submitted before review")
				}
				options.Policy, options.Confirm = policy, true
			}
			err := runMigrationPipeline(context.Background(), options, command, fetch, io.Discard, io.Discard)
			if interruption == "stage" {
				if err == nil || !strings.Contains(err.Error(), "unresolved attempt") || calls["stage"] != 1 {
					t.Fatalf("uncertain stage retried: %v, %v", err, calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"export-git", "stage"} {
				if err := os.Remove(filepath.Join(work, name+".done.json")); err != nil {
					t.Fatal(err)
				}
			}
			if err := runMigrationPipeline(context.Background(), options, command, fetch, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			wantDownloads := 2
			if interruption == "download" {
				wantDownloads++
			}
			if calls["export"] != 2 || calls["download"] != wantDownloads || calls["stage"] != 1 {
				t.Fatalf("repeated completed mutation: %v", calls)
			}
			if err := os.WriteFile(filepath.Join(work, "original-git.tar.gz"), []byte("tampered"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := runMigrationPipeline(context.Background(), options, command, fetch, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "differs") {
				t.Fatalf("archive mutation accepted: %v", err)
			}
		})
	}
}

func TestWaitForExport(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		states    []string
		wantError bool
	}{
		{"complete", []string{"pending", "exporting", "exported"}, false},
		{"failed", []string{"failed"}, true},
		{"unknown", []string{"unknown"}, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			index := 0
			err := waitForExport(context.Background(), "git", time.Millisecond, time.Second, io.Discard, func(context.Context) (string, error) {
				state := scenario.states[index]
				index++
				return state, nil
			})
			if (err != nil) != scenario.wantError {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForExport(ctx, "git", time.Hour, time.Hour, io.Discard, func(context.Context) (string, error) { t.Fatal("polled after cancellation"); return "", nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := waitForExport(context.Background(), "git", time.Hour, time.Millisecond, io.Discard, func(context.Context) (string, error) { return "pending", nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestRecoverRecordedStep(t *testing.T) {
	work := t.TempDir()
	receipt := filepath.Join(work, "receipt.json")
	if err := migrate.WriteJSON(receipt, map[string]string{"id": "known-id"}); err != nil {
		t.Fatal(err)
	}
	if err := migrate.WriteJSON(filepath.Join(work, "stage.attempt.json"), map[string]string{"step": "stage"}); err != nil {
		t.Fatal(err)
	}
	if err := recoverRecordedStep(work, "stage", receipt, func() error { return errors.New("conflicting destination") }); err == nil {
		t.Fatal("accepted invalid receipt")
	}
	if err := recoverRecordedStep(work, "stage", receipt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := executeMigrationSteps(context.Background(), work, io.Discard, []migrationStep{{"stage", true, func(context.Context) error { t.Fatal("resubmitted known migration"); return nil }}}); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationOptionGuards(t *testing.T) {
	base := migrationOptions{SourceURL: "https://github.com/source/repo", SourceAPI: "https://api.github.com", TargetAPI: "https://api.github.com", UploadAPI: "https://uploads.github.com", TargetOrg: "target", StagingRepo: "repo-staging", Threshold: migrate.DefaultThreshold, MaxBytes: 200 << 30, MaxFiles: 2000000, PollInterval: time.Second, WaitTimeout: time.Minute}
	if err := validateMigrationOptions(base); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*migrationOptions){
		func(options *migrationOptions) { options.SourceURL = "https://user:secret@github.com/source/repo" },
		func(options *migrationOptions) { options.SourceURL += ".git" },
		func(options *migrationOptions) { options.SourceURL += "?token=secret" },
		func(options *migrationOptions) { options.TargetAPI = "https://unrecognized.example" },
		func(options *migrationOptions) { options.UploadAPI += "/wrong-path" },
		func(options *migrationOptions) { options.StagingRepo = "production" },
		func(options *migrationOptions) { options.Threshold = 0 },
		func(options *migrationOptions) { options.PollInterval = 0 },
	} {
		options := base
		change(&options)
		if err := validateMigrationOptions(options); err == nil {
			t.Fatal("accepted invalid migration options")
		}
	}
}

func TestMigrationStepsResume(t *testing.T) {
	work := t.TempDir()
	exports, downloads := 0, 0
	steps := []migrationStep{
		{"export", true, func(context.Context) error { exports++; return nil }},
		{"download", false, func(context.Context) error {
			downloads++
			if downloads == 1 {
				return errors.New("interrupted")
			}
			return nil
		}},
	}
	if err := executeMigrationSteps(context.Background(), work, io.Discard, steps); err == nil {
		t.Fatal("expected interruption")
	}
	if err := executeMigrationSteps(context.Background(), work, io.Discard, steps); err != nil {
		t.Fatal(err)
	}
	if exports != 1 || downloads != 2 {
		t.Fatalf("exports=%d downloads=%d", exports, downloads)
	}
}

func TestMigrationStepsUncertainRemoteWrite(t *testing.T) {
	work := t.TempDir()
	calls := 0
	steps := []migrationStep{{"import", true, func(context.Context) error {
		calls++
		return errors.New("connection lost after submission")
	}}}
	for attempt := 0; attempt < 2; attempt++ {
		if err := executeMigrationSteps(context.Background(), work, io.Discard, steps); err == nil {
			t.Fatal("accepted uncertain remote write")
		}
	}
	if calls != 1 {
		t.Fatal("repeated uncertain mutation")
	}
}

func TestMigrationStepsRejectCorruptCheckpoint(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "export.done.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := executeMigrationSteps(context.Background(), work, io.Discard, []migrationStep{{"export", true, func(context.Context) error {
		t.Fatal("called export with corrupt checkpoint")
		return nil
	}}}); err == nil {
		t.Fatal("accepted corrupt checkpoint")
	}
}

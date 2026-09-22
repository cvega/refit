package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"refit/internal/migrate"
)

type migrationOptions struct {
	Work, SourceURL, SourceAPI, TargetAPI, UploadAPI string
	TargetOrg, StagingRepo, Policy, LFSObjects       string
	Threshold, MaxBytes                              int64
	MaxFiles                                         int
	Confirm, RouteReviewed                           bool
	PollInterval, WaitTimeout                        time.Duration
}

type migrationCommand func(context.Context, []string, io.Writer, io.Writer) error

func runMigration(ctx context.Context, args []string, output, diagnostics io.Writer) error {
	options := migrationOptions{}
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	flags.StringVar(&options.Work, "work", "", "private workflow directory; reuse to resume")
	flags.StringVar(&options.SourceURL, "source-url", "", "source HTTPS repository URL")
	flags.StringVar(&options.SourceAPI, "source-api", "https://api.github.com", "source REST origin; GHES requires its /api/v3 endpoint")
	flags.StringVar(&options.TargetAPI, "target-api", "https://api.github.com", "destination API origin")
	flags.StringVar(&options.UploadAPI, "upload-api", "https://uploads.github.com", "approved destination archive upload origin")
	flags.StringVar(&options.TargetOrg, "target-org", "", "destination organization")
	flags.StringVar(&options.StagingRepo, "staging-repo", "", "new repository with an explicit -staging name")
	flags.StringVar(&options.Policy, "policy", "", "reviewed metadata policy; may be supplied when resuming")
	flags.StringVar(&options.LFSObjects, "lfs-objects", "", "existing LFS cache; otherwise fetch payloads from source-url")
	flags.Int64Var(&options.Threshold, "threshold-bytes", migrate.DefaultThreshold, "largest allowed Git blob in bytes")
	flags.Int64Var(&options.MaxBytes, "max-extracted-bytes", 200<<30, "decompressed archive budget")
	flags.IntVar(&options.MaxFiles, "max-files", 2000000, "archive member budget")
	flags.BoolVar(&options.Confirm, "confirm-staging", false, "approve private staging import and subsequent LFS upload")
	flags.BoolVar(&options.RouteReviewed, "archive-route-reviewed", false, "confirm rewritten-archive route approval")
	flags.DurationVar(&options.PollInterval, "poll-interval", 10*time.Second, "status check interval")
	flags.DurationVar(&options.WaitTimeout, "wait-timeout", 30*time.Minute, "timeout for each wait; rerun to resume")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || options.Work == "" {
		return errors.New("migrate requires -work and no positional arguments; see migrate -help")
	}
	work, err := filepath.Abs(options.Work)
	if err != nil {
		return err
	}
	options.Work = work
	settings := filepath.Join(work, "migration.json")
	var saved migrationOptions
	if err := migrate.ReadJSON(settings, &saved); err == nil {
		if saved.Work != work {
			return errors.New("workflow directory moved; restore its original location before resuming")
		}
		var conflict bool
		flags.Visit(func(option *flag.Flag) {
			switch option.Name {
			case "work", "policy", "lfs-objects", "confirm-staging", "archive-route-reviewed", "poll-interval", "wait-timeout":
			default:
				conflict = true
			}
		})
		if conflict {
			return errors.New("resume with -work only, plus policy, LFS cache, approvals, or wait settings; source and destination are saved")
		}
		flags.Visit(func(option *flag.Flag) {
			switch option.Name {
			case "policy":
				saved.Policy = options.Policy
			case "lfs-objects":
				saved.LFSObjects = options.LFSObjects
			case "confirm-staging":
				saved.Confirm = options.Confirm
			case "archive-route-reviewed":
				saved.RouteReviewed = options.RouteReviewed
			case "poll-interval":
				saved.PollInterval = options.PollInterval
			case "wait-timeout":
				saved.WaitTimeout = options.WaitTimeout
			}
		})
		options = saved
	} else if _, statErr := os.Lstat(settings); !os.IsNotExist(statErr) {
		return errors.New("cannot read saved migration settings")
	}
	if err := validateMigrationOptions(options); err != nil {
		return err
	}
	for _, path := range []*string{&options.Policy, &options.LFSObjects} {
		if *path != "" {
			*path, err = filepath.Abs(*path)
			if err != nil {
				return err
			}
		}
	}
	sourceToken, targetToken := os.Getenv("GH_SOURCE_PAT"), os.Getenv("GH_PAT")
	if sourceToken == "" || targetToken == "" {
		return errors.New("migrate requires GH_SOURCE_PAT and GH_PAT in this terminal; credentials are never saved in the workspace")
	}
	if err := os.MkdirAll(work, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(work, "migration.lock"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("workflow is locked; stop other runs and reconcile a stale lock before resuming")
	}
	lock.Close()
	defer os.Remove(filepath.Join(work, "migration.lock"))
	if saved.Work == "" {
		if err := migrate.WriteJSON(settings, options); err != nil {
			return err
		}
	} else {
		pending, err := os.MkdirTemp(work, "settings-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(pending)
		updated := filepath.Join(pending, "migration.json")
		if err := migrate.WriteJSON(updated, options); err != nil {
			return err
		}
		if err := os.Rename(updated, settings); err != nil {
			return err
		}
	}
	sourceURL, _ := url.Parse(options.SourceURL)
	parts := strings.Split(strings.Trim(sourceURL.Path, "/"), "/")
	fmt.Fprintln(diagnostics, "Preflight: checking source and destination credentials")
	source := &migrate.API{BaseURL: options.SourceAPI, Token: sourceToken}
	target := &migrate.API{BaseURL: options.TargetAPI, UploadURL: options.UploadAPI, Token: targetToken}
	if err := source.CheckMigrationAccess(ctx, parts[0], false); err != nil {
		return fmt.Errorf("source preflight: %w", err)
	}
	if err := target.CheckMigrationAccess(ctx, options.TargetOrg, true); err != nil {
		return fmt.Errorf("destination preflight: %w", err)
	}
	if _, err := os.Lstat(filepath.Join(work, "stage.attempt.json")); os.IsNotExist(err) {
		exists, err := target.RepositoryExists(ctx, options.TargetOrg, options.StagingRepo)
		if err != nil {
			return err
		}
		if exists {
			return errors.New("destination already exists; reconcile any incomplete staging attempt before resuming")
		}
	}
	if err := runMigrationPipeline(ctx, options, run, migrate.FetchSourceLFS, output, diagnostics); err != nil {
		fmt.Fprintf(diagnostics, "Progress saved in %s. Resume: refit migrate -work %q\n", work, work)
		return err
	}
	return nil
}

func validateMigrationOptions(options migrationOptions) error {
	for _, raw := range []string{options.SourceURL, options.SourceAPI, options.TargetAPI, options.UploadAPI} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" {
			return errors.New("migration endpoints must be HTTPS URLs without credentials, queries, fragments, or encoded paths")
		}
	}
	source, _ := url.Parse(options.SourceURL)
	target, _ := url.Parse(options.TargetAPI)
	if (target.Host != "api.github.com" && !(strings.HasPrefix(target.Host, "api.") && strings.HasSuffix(target.Host, ".ghe.com"))) || strings.Trim(target.Path, "/") != "" {
		return errors.New("destination must be a recognized GitHub Enterprise Cloud API origin")
	}
	upload, _ := url.Parse(options.UploadAPI)
	if strings.Trim(upload.Path, "/") != "" {
		return errors.New("upload-api must be an origin")
	}
	if strings.HasSuffix(source.Path, ".git") || strings.HasSuffix(source.Path, "/") {
		return errors.New("source-url must use https://HOST/ORG/REPO without .git or a trailing slash")
	}
	parts := strings.Split(strings.Trim(source.Path, "/"), "/")
	if len(parts) != 2 {
		return errors.New("source-url must identify one organization/repository")
	}
	for _, segment := range append(parts, options.TargetOrg, options.StagingRepo) {
		if segment == "" || segment == "." || segment == ".." || strings.Trim(segment, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != "" {
			return errors.New("invalid source or destination repository identity")
		}
	}
	if !strings.HasSuffix(options.StagingRepo, "-staging") && !strings.Contains(options.StagingRepo, "-staging-") {
		return errors.New("destination must have an explicit -staging name")
	}
	if options.Threshold <= 0 || options.MaxBytes <= 0 || options.MaxFiles <= 0 || options.PollInterval <= 0 || options.WaitTimeout <= 0 {
		return errors.New("threshold, archive limits, and wait durations must be positive")
	}
	return nil
}

func runMigrationPipeline(ctx context.Context, options migrationOptions, command migrationCommand, fetch func(context.Context, string, string, string) error, output, diagnostics io.Writer) error {
	sourceURL, _ := url.Parse(options.SourceURL)
	parts := strings.Split(strings.Trim(sourceURL.Path, "/"), "/")
	invoke := func(ctx context.Context, args ...string) error {
		return command(ctx, args, io.Discard, diagnostics)
	}
	checkpoint := func(name string, remote bool, action func(context.Context) error) error {
		return executeMigrationSteps(ctx, options.Work, diagnostics, []migrationStep{{name, remote, action}})
	}
	type exportRecord struct {
		ID   int64  `json:"export_id"`
		Kind string `json:"kind"`
	}
	exports := make(map[string]exportRecord)
	for _, kind := range []string{"git", "metadata"} {
		recordPath := filepath.Join(options.Work, kind+"-export.json")
		if err := recoverRecordedStep(options.Work, "export-"+kind, recordPath, func() error {
			var record exportRecord
			if err := migrate.ReadJSON(recordPath, &record); err != nil {
				return err
			}
			if record.ID <= 0 || record.Kind != kind {
				return errors.New("invalid saved export receipt")
			}
			return nil
		}); err != nil {
			return err
		}
		if err := checkpoint("export-"+kind, true, func(ctx context.Context) error {
			var response bytes.Buffer
			if err := command(ctx, []string{"export", "-source-api", options.SourceAPI, "-org", parts[0], "-repo", parts[1], "-kind", kind}, &response, diagnostics); err != nil {
				return err
			}
			var record exportRecord
			if err := json.Unmarshal(response.Bytes(), &record); err != nil || record.ID <= 0 || record.Kind != kind {
				return errors.New("invalid export receipt; reconcile remote export before retrying")
			}
			return migrate.WriteJSON(recordPath, record)
		}); err != nil {
			return err
		}
		var record exportRecord
		if err := migrate.ReadJSON(recordPath, &record); err != nil || record.ID <= 0 || record.Kind != kind {
			return errors.New("invalid saved export receipt")
		}
		exports[kind] = record
	}
	archives := make(map[string]string)
	for _, kind := range []string{"git", "metadata"} {
		archive := filepath.Join(options.Work, "original-"+kind+".tar.gz")
		archives[kind] = archive
		exportID := strconv.FormatInt(exports[kind].ID, 10)
		if err := checkpoint("download-"+kind, false, func(ctx context.Context) error {
			if err := waitForExport(ctx, kind, options.PollInterval, options.WaitTimeout, diagnostics, func(ctx context.Context) (string, error) {
				var response bytes.Buffer
				if err := command(ctx, []string{"export-status", "-source-api", options.SourceAPI, "-org", parts[0], "-export-id", exportID}, &response, diagnostics); err != nil {
					return "", err
				}
				var status struct {
					State string `json:"state"`
				}
				err := json.Unmarshal(response.Bytes(), &status)
				return status.State, err
			}); err != nil {
				return err
			}
			if err := invoke(ctx, "download", "-source-api", options.SourceAPI, "-org", parts[0], "-export-id", exportID, "-out", archive); err != nil {
				return err
			}
			hash, err := migrate.FileSHA256(archive)
			if err != nil {
				return err
			}
			return migrate.WriteJSON(filepath.Join(options.Work, kind+"-original.json"), map[string]string{"sha256": hash})
		}); err != nil {
			return err
		}
		var receipt struct {
			SHA256 string `json:"sha256"`
		}
		if err := migrate.ReadJSON(filepath.Join(options.Work, kind+"-original.json"), &receipt); err != nil {
			return err
		}
		hash, err := migrate.FileSHA256(archive)
		if err != nil || hash != receipt.SHA256 {
			return errors.New("original archive differs from its download receipt")
		}
	}
	limitArgs := []string{"-max-extracted-bytes", strconv.FormatInt(options.MaxBytes, 10), "-max-files", strconv.Itoa(options.MaxFiles)}
	inspectionRecord := filepath.Join(options.Work, "inspection.json")
	if err := checkpoint("inspect", false, func(ctx context.Context) error {
		directory, err := os.MkdirTemp(options.Work, "inspection-")
		if err != nil {
			return err
		}
		root := filepath.Join(directory, "git")
		args := append([]string{"inspect", "-git-archive", archives["git"], "-work", root, "-threshold-bytes", strconv.FormatInt(options.Threshold, 10)}, limitArgs...)
		if err := command(ctx, args, diagnostics, diagnostics); err != nil {
			return err
		}
		return migrate.WriteJSON(inspectionRecord, map[string]string{"root": root})
	}); err != nil {
		return err
	}
	var inspection struct {
		Root string `json:"root"`
	}
	if err := migrate.ReadJSON(inspectionRecord, &inspection); err != nil {
		return err
	}
	preparedRecord := filepath.Join(options.Work, "prepared.json")
	if err := checkpoint("prepare", false, func(ctx context.Context) error {
		if options.Policy == "" {
			return errors.New("metadata policy review required; inspect the downloaded originals and resume with -policy PATH; no import submitted")
		}
		var policy migrate.ArchivePolicy
		if err := migrate.ReadJSON(options.Policy, &policy); err != nil {
			return err
		}
		if !policy.Git.Reviewed || !policy.Metadata.Reviewed {
			return errors.New("metadata policy is not reviewed; no import submitted")
		}
		objects := options.LFSObjects
		if objects == "" {
			repositories, err := migrate.DiscoverRepositories(inspection.Root)
			if err != nil {
				return err
			}
			if len(repositories) != 1 {
				return errors.New("exactly one archived repository is required")
			}
			repo := filepath.Join(inspection.Root, filepath.FromSlash(repositories[0]))
			fmt.Fprintln(diagnostics, "LFS: fetching existing payloads from the explicit source repository (no Git refs fetched)")
			if err := fetch(ctx, repo, options.SourceURL, os.Getenv("GH_SOURCE_PAT")); err != nil {
				return err
			}
			objects = filepath.Join(repo, "lfs", "objects")
			if _, err := os.Stat(objects); os.IsNotExist(err) {
				objects = ""
			}
		}
		directory, err := os.MkdirTemp(options.Work, "preparation-")
		if err != nil {
			return err
		}
		prepared := filepath.Join(directory, "prepared")
		args := append([]string{"prepare", "-git-archive", archives["git"], "-metadata-archive", archives["metadata"], "-policy", options.Policy, "-work", prepared, "-threshold-bytes", strconv.FormatInt(options.Threshold, 10)}, limitArgs...)
		if objects != "" {
			args = append(args, "-lfs-objects", objects)
		}
		if err := invoke(ctx, args...); err != nil {
			return err
		}
		return migrate.WriteJSON(preparedRecord, map[string]string{"work": prepared})
	}); err != nil {
		return err
	}
	var prepared struct {
		Work string `json:"work"`
	}
	if err := migrate.ReadJSON(preparedRecord, &prepared); err != nil {
		return err
	}
	if err := invoke(ctx, "verify", "-work", prepared.Work); err != nil {
		return err
	}
	if !options.Confirm || !options.RouteReviewed {
		return errors.New("preparation complete; resume with -confirm-staging and -archive-route-reviewed after route and automation review")
	}
	stagingRecord := filepath.Join(prepared.Work, "staging.json")
	if err := recoverRecordedStep(options.Work, "stage", stagingRecord, func() error {
		var receipt migrate.Staging
		if err := migrate.ReadJSON(stagingRecord, &receipt); err != nil {
			return err
		}
		if receipt.MigrationID == "" || receipt.TargetAPI != options.TargetAPI || receipt.Organization != options.TargetOrg || receipt.Repository != options.StagingRepo {
			return errors.New("saved staging receipt conflicts with migration settings")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := checkpoint("stage", true, func(ctx context.Context) error {
		return invoke(ctx, "stage", "-work", prepared.Work, "-source-url", options.SourceURL, "-target-org", options.TargetOrg, "-staging-repo", options.StagingRepo, "-target-api", options.TargetAPI, "-upload-api", options.UploadAPI, "-confirm-staging", "-archive-route-reviewed")
	}); err != nil {
		return err
	}
	if err := invoke(ctx, "status", "-work", prepared.Work, "-wait", "-poll-interval", options.PollInterval.String(), "-wait-timeout", options.WaitTimeout.String()); err != nil {
		return err
	}
	if err := checkpoint("lfs-upload", false, func(ctx context.Context) error {
		return invoke(ctx, "lfs-push", "-work", prepared.Work, "-confirm-staging")
	}); err != nil {
		return err
	}
	fmt.Fprintln(diagnostics, "Migration and LFS upload complete. Review the private staging repository and Migration Log; production promotion is not performed.")
	return json.NewEncoder(output).Encode(map[string]string{"state": "SUCCEEDED", "organization": options.TargetOrg, "repository": options.StagingRepo, "prepared_work": prepared.Work, "review": "required"})
}

func recoverRecordedStep(work, name, receipt string, validate func() error) error {
	for _, path := range []string{filepath.Join(work, name+".attempt.json"), receipt} {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
	}
	done := filepath.Join(work, name+".done.json")
	if _, err := os.Lstat(done); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := validate(); err != nil {
		return err
	}
	return migrate.WriteJSON(done, map[string]string{"step": name})
}

func waitForExport(ctx context.Context, kind string, interval, timeout time.Duration, diagnostics io.Writer, check func(context.Context) (string, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, err := check(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(diagnostics, "[%s] %s export: %s\n", time.Since(started).Round(time.Second), kind, state)
		switch state {
		case "exported":
			return nil
		case "pending", "exporting":
		case "failed":
			return errors.New("export failed; inspect the saved export ID")
		default:
			return errors.New("unexpected export state")
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

type migrationStep struct {
	name   string
	remote bool
	run    func(context.Context) error
}

func executeMigrationSteps(ctx context.Context, work string, diagnostics io.Writer, steps []migrationStep) error {
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		done := filepath.Join(work, step.name+".done.json")
		var completed struct {
			Step string `json:"step"`
		}
		if err := migrate.ReadJSON(done, &completed); err == nil {
			if completed.Step != step.name {
				return errors.New("invalid migration checkpoint")
			}
			fmt.Fprintf(diagnostics, "%s: already complete\n", step.name)
			continue
		} else if _, statErr := os.Lstat(done); !os.IsNotExist(statErr) {
			return fmt.Errorf("cannot read %s checkpoint", step.name)
		}
		if step.remote {
			attempt := filepath.Join(work, step.name+".attempt.json")
			if err := migrate.WriteJSON(attempt, map[string]string{"step": step.name}); err != nil {
				return fmt.Errorf("%s has an unresolved attempt; reconcile remote state before retrying (do not delete its record)", step.name)
			}
		}
		fmt.Fprintf(diagnostics, "%s: running\n", step.name)
		if err := step.run(ctx); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
		if err := migrate.WriteJSON(done, map[string]string{"step": step.name}); err != nil {
			return fmt.Errorf("cannot record %s completion", step.name)
		}
	}
	return nil
}

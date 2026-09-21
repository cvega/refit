package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"refit/internal/migrate"
)

const usage = `refit COMMAND [flags]

  export          Start a Git or metadata REST export; prints export ID
  export-status   Check one REST export
  download        Download an exported archive without exposing signed URLs
  inspect         Extract a Git archive into a new directory and inventory blobs
  prepare         Rewrite disposable archives using a reviewed metadata policy
  verify          Verify prepared archives, refs, and every local LFS payload
  stage           Upload and enqueue a new private staging repository
  status          Check a GEI import ID
  lfs-push        Upload verified LFS payloads after the staging import succeeds

Run a command with -help for flags. Credentials: GH_SOURCE_PAT and GH_PAT.
No command pushes Git refs. Production promotion is not supported.
`

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output, diagnostics io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-help" || args[0] == "--help" {
		_, err := io.WriteString(output, usage)
		return err
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	work := flags.String("work", "", "new work directory for inspect/prepare; prepared directory for verify/stage/lfs-push")
	gitArchive := flags.String("git-archive", "", "immutable source Git tar.gz")
	metadataArchive := flags.String("metadata-archive", "", "immutable source metadata tar.gz")
	policy := flags.String("policy", "", "reviewed archive metadata policy JSON")
	lfsObjects := flags.String("lfs-objects", "", "existing LFS objects directory, containing aa/bb/full-sha256 files")
	threshold := flags.Int64("threshold-bytes", migrate.DefaultThreshold, "largest permitted Git blob in bytes; equality is allowed")
	maxBytes := flags.Int64("max-extracted-bytes", 200<<30, "maximum decompressed bytes per archive including tar overhead")
	maxFiles := flags.Int("max-files", 2000000, "maximum archive members")
	org := flags.String("org", "", "source organization")
	repo := flags.String("repo", "", "source repository name")
	kind := flags.String("kind", "git", "export kind: git or metadata")
	exportID := flags.Int64("export-id", 0, "REST export ID")
	out := flags.String("out", "", "new destination archive file")
	sourceAPI := flags.String("source-api", "https://api.github.com", "source REST base; GHES uses https://HOST/api/v3")
	targetAPI := flags.String("target-api", "https://api.github.com", "destination GraphQL/REST base")
	uploadAPI := flags.String("upload-api", "https://uploads.github.com", "destination GitHub-owned upload origin")
	sourceURL := flags.String("source-url", "", "original https://HOST/ORG/REPO URL")
	targetOrg := flags.String("target-org", "", "destination organization")
	stagingRepo := flags.String("staging-repo", "", "new repository name ending in -staging or containing -staging-")
	confirm := flags.Bool("confirm-staging", false, "approve private staging import or LFS upload")
	routeReviewed := flags.Bool("archive-route-reviewed", false, "confirm rewritten archive layout/schema and import route were reviewed")
	migrationID := flags.String("migration-id", "", "GEI repository migration node ID")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	require := func(values ...string) error {
		for _, value := range values {
			if value == "" {
				return errors.New("required flags missing; see command -help")
			}
		}
		return nil
	}
	emit := func(value any) error { return json.NewEncoder(output).Encode(value) }
	source := &migrate.API{BaseURL: *sourceAPI, Token: os.Getenv("GH_SOURCE_PAT")}
	target := &migrate.API{BaseURL: *targetAPI, UploadURL: *uploadAPI, Token: os.Getenv("GH_PAT")}
	limits := migrate.ArchiveLimits{MaxBytes: *maxBytes, MaxFiles: *maxFiles}
	switch command {
	case "export":
		if err := require(*org, *repo, source.Token); err != nil {
			return err
		}
		id, err := source.Export(ctx, *org, *repo, *kind)
		if err != nil {
			return err
		}
		return emit(map[string]any{"export_id": id, "kind": *kind})
	case "export-status", "download":
		if err := require(*org, source.Token); err != nil {
			return err
		}
		state, err := source.ExportStatus(ctx, *org, *exportID)
		if err != nil {
			return err
		}
		if command == "export-status" {
			return emit(map[string]string{"state": state})
		}
		if state != "exported" {
			return errors.New("archive is not exported yet")
		}
		if err := require(*out); err != nil {
			return err
		}
		return source.Download(ctx, *org, *exportID, *out)
	case "inspect":
		if err := require(*gitArchive, *work); err != nil {
			return err
		}
		if err := migrate.ExtractArchive(*gitArchive, *work, limits); err != nil {
			return err
		}
		root, err := filepath.EvalSymlinks(*work)
		if err != nil {
			return err
		}
		root, err = filepath.Abs(root)
		if err != nil {
			return err
		}
		repositories, err := migrate.DiscoverRepositories(root)
		if err != nil {
			return err
		}
		for _, repository := range repositories {
			report, err := migrate.InspectGit(ctx, filepath.Join(root, repository), *threshold)
			if err != nil {
				return err
			}
			if err := emit(map[string]any{"repository": repository, "report": report}); err != nil {
				return err
			}
		}
		return nil
	case "prepare":
		if err := require(*gitArchive, *metadataArchive, *work, *policy); err != nil {
			return err
		}
		report, err := migrate.Prepare(ctx, migrate.PrepareOptions{GitArchive: *gitArchive, MetadataArchive: *metadataArchive, WorkDir: *work, PolicyFile: *policy, LFSObjectsDir: *lfsObjects, Threshold: *threshold, Limits: limits})
		if err != nil {
			return err
		}
		return emit(report)
	case "verify":
		if err := require(*work); err != nil {
			return err
		}
		report, err := migrate.VerifyPrepared(ctx, *work)
		if err != nil {
			return err
		}
		return emit(report)
	case "stage":
		if err := require(*work, *sourceURL, *targetOrg, *stagingRepo, target.Token, source.Token); err != nil {
			return err
		}
		if !*confirm || !*routeReviewed {
			return errors.New("stage requires -confirm-staging and -archive-route-reviewed")
		}
		if !strings.HasSuffix(*stagingRepo, "-staging") && !strings.Contains(*stagingRepo, "-staging-") {
			return errors.New("destination must have an explicit -staging name")
		}
		parsed, err := url.Parse(*sourceURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || len(strings.Split(strings.Trim(parsed.Path, "/"), "/")) != 2 {
			return errors.New("source-url must be an HTTPS repository URL without credentials or query")
		}
		if _, err := migrate.VerifyPrepared(ctx, *work); err != nil {
			return err
		}
		exists, err := target.RepositoryExists(ctx, *targetOrg, *stagingRepo)
		if err != nil {
			return err
		}
		if exists {
			return errors.New("staging destination already exists; nothing uploaded")
		}
		if err := migrate.WriteJSON(filepath.Join(*work, "staging-attempt.json"), map[string]string{"target_org": *targetOrg, "repository": *stagingRepo, "source_url": *sourceURL}); err != nil {
			return errors.New("cannot reserve staging attempt; inspect any existing attempt before retrying")
		}
		ownerID, databaseID, err := target.Organization(ctx, *targetOrg)
		if err != nil {
			return err
		}
		gitURI, err := target.Upload(ctx, databaseID, filepath.Join(*work, "git-rewritten.tar.gz"))
		if err != nil {
			return err
		}
		metadataURI, err := target.Upload(ctx, databaseID, filepath.Join(*work, "metadata-rewritten.tar.gz"))
		if err != nil {
			return err
		}
		sourceID, err := target.CreateSource(ctx, "refit-staging", parsed.Scheme+"://"+parsed.Host, ownerID)
		if err != nil {
			return err
		}
		id, err := target.StartImport(ctx, sourceID, ownerID, *stagingRepo, *sourceURL, gitURI, metadataURI, source.Token)
		if err != nil {
			return err
		}
		state := migrate.Staging{MigrationID: id, TargetAPI: *targetAPI, Organization: *targetOrg, Repository: *stagingRepo}
		if err := migrate.WriteJSON(filepath.Join(*work, "staging.json"), state); err != nil {
			_ = emit(state)
			return err
		}
		return emit(state)
	case "status":
		if err := require(*migrationID, target.Token); err != nil {
			return err
		}
		state, failure, err := target.ImportStatus(ctx, *migrationID)
		if err != nil {
			return err
		}
		return emit(map[string]string{"state": state, "failure": failure})
	case "lfs-push":
		if err := require(*work, target.Token); err != nil {
			return err
		}
		if !*confirm {
			return errors.New("LFS upload requires -confirm-staging")
		}
		return migrate.PushStagingLFS(ctx, *work, target.Token)
	default:
		return errors.New("unknown command; run refit help")
	}
}

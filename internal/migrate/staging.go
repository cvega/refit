package migrate

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Staging struct {
	MigrationID  string `json:"migration_id"`
	TargetAPI    string `json:"target_api"`
	Organization string `json:"organization"`
	Repository   string `json:"repository"`
}

func stagingURL(stage Staging) (string, error) {
	parsed, err := url.Parse(stage.TargetAPI)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("unsupported staging API origin")
	}
	host := ""
	if parsed.Host == "api.github.com" {
		host = "github.com"
	} else if strings.HasPrefix(parsed.Host, "api.") && strings.HasSuffix(parsed.Host, ".ghe.com") {
		host = strings.TrimPrefix(parsed.Host, "api.")
	}
	if host == "" {
		return "", errors.New("LFS upload requires a recognized GitHub Enterprise Cloud API origin")
	}
	for _, segment := range []string{stage.Organization, stage.Repository} {
		if segment == "" || strings.Trim(segment, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != "" || segment == "." || segment == ".." {
			return "", errors.New("invalid staging repository identity")
		}
	}
	if !strings.HasSuffix(stage.Repository, "-staging") && !strings.Contains(stage.Repository, "-staging-") {
		return "", errors.New("not a staging repository")
	}
	return "https://" + host + "/" + stage.Organization + "/" + stage.Repository + ".git", nil
}

func PushStagingLFS(ctx context.Context, work, token string) error {
	if token == "" {
		return errors.New("GH_PAT is required")
	}
	prepared, err := VerifyPrepared(ctx, work)
	if err != nil {
		return err
	}
	var stage Staging
	if err := ReadJSON(filepath.Join(work, "staging.json"), &stage); err != nil {
		return err
	}
	targetURL, err := stagingURL(stage)
	if err != nil {
		return err
	}
	api := &API{BaseURL: stage.TargetAPI, Token: token}
	state, _, err := api.ImportStatus(ctx, stage.MigrationID)
	if err != nil {
		return err
	}
	if state != "SUCCEEDED" {
		return errors.New("staging import must succeed before uploading LFS payloads")
	}
	endpoint, err := api.endpoint("/repos/" + stage.Organization + "/" + stage.Repository)
	if err != nil {
		return err
	}
	var repository struct {
		Private bool   `json:"private"`
		HTMLURL string `json:"html_url"`
	}
	if err := api.jsonRequest(ctx, "GET", endpoint, nil, &repository); err != nil {
		return err
	}
	if !repository.Private || !strings.EqualFold(repository.HTMLURL, strings.TrimSuffix(targetURL, ".git")) {
		return errors.New("destination is not the expected private staging repository")
	}
	root, err := filepath.EvalSymlinks(work)
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	repo := filepath.Join(root, "git", filepath.FromSlash(prepared.Repository))
	if len(prepared.Report.LFSObjects) != 0 {
		if err := pushLFSPayloads(ctx, repo, targetURL, token, prepared.Report.LFSObjects); err != nil {
			return err
		}
	}
	var receipt Staging
	if err := ReadJSON(filepath.Join(root, "lfs-uploaded.json"), &receipt); err == nil {
		if receipt != stage {
			return errors.New("LFS upload receipt conflicts with the staging destination")
		}
		return nil
	} else if _, statErr := os.Lstat(filepath.Join(root, "lfs-uploaded.json")); !os.IsNotExist(statErr) {
		return errors.New("cannot read LFS upload receipt")
	}
	return WriteJSON(filepath.Join(root, "lfs-uploaded.json"), stage)
}

func pushLFSPayloads(ctx context.Context, repo, targetURL, token string, oids []string) error {
	cmd := gitCommand(ctx, repo, "-c", "lfs.url="+targetURL+"/info/lfs", "-c", "lfs.pushurl="+targetURL+"/info/lfs", "lfs", "push", "--object-id", "--stdin", targetURL)
	cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http."+targetURL+"/.extraheader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
	cmd.Stdin = strings.NewReader(strings.Join(oids, "\n") + "\n")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		return gitFailure(ctx, "staging LFS upload")
	}
	return nil
}

func FetchSourceLFS(ctx context.Context, repo, sourceURL, token string) error {
	parsed, err := url.Parse(sourceURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" || len(strings.Split(strings.Trim(parsed.Path, "/"), "/")) != 2 || token == "" {
		return errors.New("LFS fetch requires a source HTTPS repository URL and GH_SOURCE_PAT")
	}
	if _, err := gitScan(ctx, repo, DefaultThreshold); err != nil {
		return err
	}
	sourceURL = strings.TrimSuffix(strings.TrimSuffix(sourceURL, "/"), ".git") + ".git"
	return fetchSourceLFSPayloads(ctx, repo, sourceURL, token)
}

func fetchSourceLFSPayloads(ctx context.Context, repo, sourceURL, token string) error {
	cmd := gitCommand(ctx, repo, "-c", "credential.helper=", "-c", "remote.origin.url="+sourceURL, "-c", "lfs.url="+sourceURL+"/info/lfs", "lfs", "fetch", "--all", "origin")
	cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http."+sourceURL+"/.extraheader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		return gitFailure(ctx, "source LFS fetch (Git refs were not fetched)")
	}
	_, err := VerifyLFS(ctx, repo)
	return err
}

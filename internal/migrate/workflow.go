package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
)

type PrepareOptions struct {
	GitArchive, MetadataArchive, WorkDir, PolicyFile string
	LFSObjectsDir                                    string
	Threshold                                        int64
	Limits                                           ArchiveLimits
}

type ArchivePolicy struct {
	Git      MetadataPolicy `json:"git"`
	Metadata MetadataPolicy `json:"metadata"`
}

type Prepared struct {
	Version         int               `json:"version"`
	Threshold       int64             `json:"threshold"`
	Repository      string            `json:"repository"`
	Hashes          map[string]string `json:"hashes"`
	Report          GitReport         `json:"report"`
	MetadataChanges int               `json:"metadata_changes"`
}

func ReadJSON(filename string, value any) error {
	data, _, err := readMetadataJSON(filename)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func WriteJSON(filename string, value any) error {
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return errors.Join(encoder.Encode(value), file.Close())
}

func Prepare(ctx context.Context, options PrepareOptions) (*Prepared, error) {
	if options.Threshold <= 0 {
		return nil, errors.New("threshold must be positive")
	}
	var policy ArchivePolicy
	if err := ReadJSON(options.PolicyFile, &policy); err != nil {
		return nil, err
	}
	for _, item := range []MetadataPolicy{policy.Git, policy.Metadata} {
		if _, err := compileMetadataPolicy(item); err != nil {
			return nil, err
		}
	}
	work, err := canonicalLocation(options.WorkDir)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(work, 0700); err != nil {
		return nil, err
	}
	prepared := &Prepared{Version: 1, Threshold: options.Threshold, Hashes: map[string]string{}}
	sources := map[string]string{"git": options.GitArchive, "metadata": options.MetadataArchive}
	for name, source := range sources {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		hash, err := FileSHA256(source)
		if err != nil {
			return nil, err
		}
		prepared.Hashes[name+"-original"] = hash
		if err := ExtractArchive(source, filepath.Join(work, name), options.Limits); err != nil {
			return nil, err
		}
	}
	gitRoot, metadataRoot := filepath.Join(work, "git"), filepath.Join(work, "metadata")
	repositories, err := DiscoverRepositories(gitRoot)
	if err != nil {
		return nil, err
	}
	if len(repositories) != 1 {
		return nil, errors.New("exactly one archived bare repository is required; multi-repository/wiki layouts need a reviewed adapter")
	}
	metadataFiles, err := archiveTree(metadataRoot)
	if err != nil {
		return nil, err
	}
	if len(reposInTree(metadataFiles)) != 0 {
		return nil, errors.New("metadata archive must not contain Git repositories")
	}
	prepared.Repository = repositories[0]
	repo := filepath.Join(gitRoot, filepath.FromSlash(prepared.Repository))
	if options.LFSObjectsDir != "" {
		if err := copyLFSCache(options.LFSObjectsDir, filepath.Join(repo, "lfs", "objects")); err != nil {
			return nil, err
		}
	}
	prepared.Report, err = RewriteGit(ctx, repo, options.Threshold, filepath.Join(work, "commit-map.csv"))
	if err != nil {
		return nil, err
	}
	for _, item := range []struct {
		root   string
		policy MetadataPolicy
	}{{gitRoot, policy.Git}, {metadataRoot, policy.Metadata}} {
		count, err := RewriteMetadata(item.root, item.policy, prepared.Report.CommitMap)
		if err != nil {
			return nil, err
		}
		prepared.MetadataChanges += count
	}
	for name, source := range sources {
		var exclusions []string
		if name == "git" {
			for _, child := range []string{"lfs", "hooks", "logs"} {
				exclusions = append(exclusions, filepath.ToSlash(filepath.Join(prepared.Repository, child)))
			}
		}
		output := name + "-rewritten.tar.gz"
		if err := RepackArchive(source, filepath.Join(work, name), filepath.Join(work, output), exclusions); err != nil {
			return nil, err
		}
		hash, err := FileSHA256(source)
		if err != nil {
			return nil, err
		}
		if hash != prepared.Hashes[name+"-original"] {
			return nil, errors.New("source archive changed during preparation")
		}
		prepared.Hashes[output], err = FileSHA256(filepath.Join(work, output))
		if err != nil {
			return nil, err
		}
	}
	prepared.Hashes["commit-map.csv"], err = FileSHA256(filepath.Join(work, "commit-map.csv"))
	if err != nil {
		return nil, err
	}
	check := filepath.Join(work, "repack-check")
	if err := ExtractArchive(filepath.Join(work, "git-rewritten.tar.gz"), check, options.Limits); err != nil {
		return nil, err
	}
	defer os.RemoveAll(check)
	inventory, err := InspectGit(ctx, filepath.Join(check, filepath.FromSlash(prepared.Repository)), options.Threshold)
	if err != nil {
		return nil, err
	}
	if len(inventory.LargeBefore) != 0 || !reflect.DeepEqual(inventory.RefsBefore, prepared.Report.RefsAfter) {
		return nil, errors.New("repacked Git archive failed verification")
	}
	if err := WriteJSON(filepath.Join(work, "report.json"), prepared); err != nil {
		return nil, err
	}
	return prepared, nil
}

func LoadPrepared(work string) (*Prepared, error) {
	var prepared Prepared
	if err := ReadJSON(filepath.Join(work, "report.json"), &prepared); err != nil {
		return nil, err
	}
	rel, err := safeRelativePath(prepared.Repository)
	if err != nil || rel != prepared.Repository || prepared.Version != 1 || prepared.Threshold <= 0 {
		return nil, errors.New("invalid preparation report")
	}
	return &prepared, nil
}

func copyLFSCache(source, destination string) error {
	files, err := archiveTree(source)
	if err != nil {
		return err
	}
	for relative, info := range files {
		if info.IsDir() {
			continue
		}
		oid := filepath.Base(relative)
		if len(oid) != 64 || !fullGitSHA(oid) || relative != oid[:2]+"/"+oid[2:4]+"/"+oid {
			return errors.New("LFS cache must contain only aa/bb/full-sha256 object files")
		}
		path := filepath.Join(destination, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		input, err := openRegular(filepath.Join(source, filepath.FromSlash(relative)))
		if err != nil {
			return err
		}
		output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		if err := errors.Join(copyErr, input.Close(), output.Close()); err != nil {
			return err
		}
	}
	return nil
}

func VerifyPrepared(ctx context.Context, work string) (*Prepared, error) {
	work, err := canonicalLocation(work)
	if err != nil {
		return nil, err
	}
	prepared, err := LoadPrepared(work)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"git-rewritten.tar.gz", "metadata-rewritten.tar.gz", "commit-map.csv"} {
		hash, err := FileSHA256(filepath.Join(work, name))
		if err != nil {
			return nil, err
		}
		if prepared.Hashes[name] != hash {
			return nil, fmt.Errorf("prepared artifact changed: %s", name)
		}
	}
	repo := filepath.Join(work, "git", filepath.FromSlash(prepared.Repository))
	inventory, err := InspectGit(ctx, repo, prepared.Threshold)
	if err != nil {
		return nil, err
	}
	if len(inventory.LargeBefore) != 0 || !reflect.DeepEqual(inventory.RefsBefore, prepared.Report.RefsAfter) {
		return nil, errors.New("prepared Git refs or blob inventory changed")
	}
	oids, err := VerifyLFS(ctx, repo)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(oids, prepared.Report.LFSObjects) {
		return nil, errors.New("prepared LFS inventory changed")
	}
	return prepared, nil
}

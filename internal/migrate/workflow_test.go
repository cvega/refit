package migrate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareArchiveToVerifiedArtifacts(t *testing.T) {
	gitTestLFS(t)
	fixture := gitTestFixture(t)
	directory := gitTestRoot(t)
	gitArchive := filepath.Join(directory, "original-git.tar.gz")
	metadataArchive := filepath.Join(directory, "original-metadata.tar.gz")
	if err := PackArchive(fixture.repo, gitArchive); err != nil {
		t.Fatal(err)
	}
	metadataRoot := filepath.Join(directory, "source-metadata")
	if err := os.Mkdir(metadataRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(metadataRoot, "pulls.json"), []map[string]string{{"head": fixture.hidden}}); err != nil {
		t.Fatal(err)
	}
	if err := PackArchive(metadataRoot, metadataArchive); err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(directory, "policy.json")
	policy := ArchivePolicy{Git: MetadataPolicy{Reviewed: true}, Metadata: metadataTestPolicy("pulls.json", MetadataRule{Path: "/*/head", Action: "commit"})}
	if err := WriteJSON(policyPath, policy); err != nil {
		t.Fatal(err)
	}
	gitHash, _ := FileSHA256(gitArchive)
	metadataHash, _ := FileSHA256(metadataArchive)
	work := filepath.Join(directory, "work")
	options := PrepareOptions{GitArchive: gitArchive, MetadataArchive: metadataArchive, WorkDir: work, PolicyFile: policyPath, Threshold: 512, Limits: ArchiveLimits{MaxBytes: 10 << 20, MaxFiles: 1000}}
	report, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if report.MetadataChanges != 1 || report.Report.RefsAfter["refs/pull/17/head"] == fixture.hidden {
		t.Fatal("hidden history or metadata not rewritten")
	}
	if _, err := VerifyPrepared(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if hash, _ := FileSHA256(gitArchive); hash != gitHash {
		t.Fatal("source Git archive changed")
	}
	if hash, _ := FileSHA256(metadataArchive); hash != metadataHash {
		t.Fatal("source metadata archive changed")
	}
	if _, err := Prepare(context.Background(), options); err == nil {
		t.Fatal("existing work overwritten")
	}
	var pulls []map[string]string
	if err := ReadJSON(filepath.Join(work, "metadata", "pulls.json"), &pulls); err != nil {
		t.Fatal(err)
	}
	if pulls[0]["head"] != report.Report.CommitMap[fixture.hidden] {
		t.Fatal("metadata map mismatch")
	}
	gitTestAppend(t, filepath.Join(work, "git-rewritten.tar.gz"), "tampered")
	if _, err := VerifyPrepared(context.Background(), work); err == nil {
		t.Fatal("tampered archive accepted")
	}
}

func TestLoadPreparedRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	if err := WriteJSON(filepath.Join(root, "report.json"), Prepared{Version: 1, Threshold: DefaultThreshold, Repository: "../outside"}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrepared(root); err == nil {
		t.Fatal("unsafe repository path accepted")
	}
}

package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func gitTestRoot(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is required for integration tests")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func gitTestExec(t *testing.T, repo string, input []byte, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir=" + repo, "-c", "core.hooksPath=" + os.DevNull}, args...)...)
	cmd.Env = append(gitEnv(), "GIT_AUTHOR_NAME=Archive Test", "GIT_AUTHOR_EMAIL=archive@example.invalid", "GIT_COMMITTER_NAME=Archive Test", "GIT_COMMITTER_EMAIL=archive@example.invalid", "GIT_AUTHOR_DATE=2020-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2020-01-01T00:00:00Z")
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSuffix(string(output), "\n")
}

func gitTestLFS(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("Git LFS is not installed")
	}
	cmd := exec.Command("git", "lfs", "version")
	cmd.Env = gitEnv()
	if err := cmd.Run(); err != nil {
		t.Fatal("installed Git LFS is not working")
	}
}

func gitTestRepo(t *testing.T) (root, repo string) {
	t.Helper()
	root = gitTestRoot(t)
	repo = filepath.Join(root, "archive.git")
	gitTestExec(t, repo, nil, "init", "--bare", "--initial-branch=main", repo)
	return root, repo
}

func gitTestBlob(t *testing.T, repo string, body []byte) string {
	t.Helper()
	return gitTestExec(t, repo, body, "hash-object", "-w", "--stdin")
}

func gitTestCommit(t *testing.T, repo, parent string, blobs map[string]string) string {
	t.Helper()
	var tree strings.Builder
	for _, name := range gitRefKeys(blobs) {
		fmt.Fprintf(&tree, "100644 blob %s\t%s\n", blobs[name], name)
	}
	treeOID := gitTestExec(t, repo, []byte(tree.String()), "mktree")
	args := []string{"commit-tree", treeOID}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return gitTestExec(t, repo, []byte("archived commit\n"), args...)
}

type gitFixture struct {
	root, repo, storage, mapPath string
	main, hidden, remote, tag    string
	boundary, big, remoteBig     string
}

func gitTestFixture(t *testing.T) gitFixture {
	t.Helper()
	root, repo := gitTestRepo(t)
	f := gitFixture{root: root, repo: repo, storage: filepath.Join(repo, "lfs"), mapPath: filepath.Join(root, "commit-map.csv")}
	small := gitTestBlob(t, repo, []byte("main stays identical\n"))
	f.boundary = gitTestBlob(t, repo, bytes.Repeat([]byte("b"), 512))
	f.main = gitTestCommit(t, repo, "", map[string]string{"readme.txt": small, "boundary.bin": f.boundary})
	f.big = gitTestBlob(t, repo, bytes.Repeat([]byte("p"), 513))
	f.hidden = gitTestCommit(t, repo, "", map[string]string{"readme.txt": small, "boundary.bin": f.boundary, "pr.bin": f.big})
	f.remoteBig = gitTestBlob(t, repo, bytes.Repeat([]byte("r"), 700))
	f.remote = gitTestCommit(t, repo, f.main, map[string]string{"readme.txt": small, "remote.bin": f.remoteBig})
	f.tag = gitTestExec(t, repo, []byte("object "+f.hidden+"\ntype commit\ntag archived-v1\ntagger Archive Test <archive@example.invalid> 1577836800 +0000\n\nOriginal annotation\n"), "hash-object", "-t", "tag", "-w", "--stdin")
	for ref, oid := range map[string]string{
		"refs/heads/main": f.main, "refs/pull/17/head": f.hidden,
		"refs/remotes/origin/only": f.remote, "refs/custom/snapshot": f.hidden,
		"refs/tags/archived-v1": f.tag, "refs/tags/light": f.hidden,
	} {
		gitTestExec(t, repo, nil, "update-ref", ref, oid)
	}
	gitTestExec(t, repo, nil, "config", "remote.origin.url", "https://source.invalid/source.git")
	gitTestExec(t, repo, nil, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	gitTestExec(t, repo, nil, "config", "remote.origin.mirror", "true")
	// Exercise removal of oversized objects from existing packs, not only loose objects.
	gitTestExec(t, repo, nil, "repack", "-ad")
	return f
}

func TestGitInventoryAllStoredObjectsAndThreshold(t *testing.T) {
	f := gitTestFixture(t)
	dangling := gitTestBlob(t, f.repo, bytes.Repeat([]byte("d"), 900))
	config, err := os.ReadFile(filepath.Join(f.repo, "config"))
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := InspectGit(context.Background(), f.repo, 512)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.RefsBefore) != 6 || inventory.RefsBefore["refs/pull/17/head"] != f.hidden || inventory.RefsBefore["refs/remotes/origin/only"] != f.remote {
		t.Fatalf("missing refs: %+v", inventory.RefsBefore)
	}
	large := make(map[string]int64)
	for _, blob := range inventory.LargeBefore {
		large[blob.OID] = blob.Size
	}
	if !reflect.DeepEqual(large, map[string]int64{f.big: 513, f.remoteBig: 700, dangling: 900}) {
		t.Fatalf("wrong strict threshold/all-object inventory: %+v", large)
	}
	unchanged, _ := os.ReadFile(filepath.Join(f.repo, "config"))
	if !bytes.Equal(config, unchanged) {
		t.Fatal("Inventory mutated config")
	}
	for _, threshold := range []int64{1_000_000_000, 1 << 62} {
		inv, err := InspectGit(context.Background(), f.repo, threshold)
		if err != nil || len(inv.LargeBefore) != 0 {
			t.Fatalf("int64 threshold %d: %+v, %v", threshold, inv, err)
		}
	}
	if _, err := InspectGit(context.Background(), f.repo, -1); err == nil {
		t.Fatal("negative threshold accepted")
	}
}

func TestGitRewriteAllArchivedRefs(t *testing.T) {
	gitTestLFS(t)
	f := gitTestFixture(t)
	result, err := RewriteGit(context.Background(), f.repo, 512, f.mapPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CommitMap) != 3 || result.CommitMap[f.main] != f.main || result.CommitMap[f.hidden] == f.hidden || result.CommitMap[f.remote] == f.remote {
		t.Fatalf("incomplete or incorrect commit map: %+v", result.CommitMap)
	}
	if !reflect.DeepEqual(gitRefKeys(result.RefsBefore), gitRefKeys(result.RefsAfter)) {
		t.Fatalf("ref names changed: before=%v after=%v", result.RefsBefore, result.RefsAfter)
	}
	for _, ref := range []string{"refs/pull/17/head", "refs/custom/snapshot", "refs/tags/light"} {
		if result.RefsAfter[ref] != result.CommitMap[f.hidden] {
			t.Fatalf("unmapped hidden ref %s", ref)
		}
	}
	for _, ref := range []string{"refs/remotes/origin/only"} {
		if result.RefsAfter[ref] != result.CommitMap[f.remote] {
			t.Fatalf("unmapped remote ref %s", ref)
		}
	}
	if result.RefsAfter["refs/heads/main"] != f.main {
		t.Fatal("unaffected main changed")
	}
	if got := gitTestExec(t, f.repo, nil, "rev-parse", "refs/pull/17/head:boundary.bin"); got != f.boundary {
		t.Fatal("equal-boundary blob was rewritten")
	}
	tag := gitTestExec(t, f.repo, nil, "cat-file", "tag", result.RefsAfter["refs/tags/archived-v1"])
	// gitTestExec trims the final newline from command output.
	if !strings.HasPrefix(tag, "object "+result.CommitMap[f.hidden]+"\n") || !strings.HasSuffix(tag, "Original annotation") {
		t.Fatalf("annotation or target not preserved: %q", tag)
	}
	for _, entry := range []struct {
		ref, name, oid string
		size           int64
	}{{"refs/pull/17/head", "pr.bin", f.big, 513}, {"refs/remotes/origin/only", "remote.bin", f.remoteBig, 700}} {
		body := gitTestExec(t, f.repo, nil, "show", entry.ref+":"+entry.name) + "\n"
		pointer, err := gitParsePointer([]byte(body))
		if err != nil || pointer == nil || pointer.Size != entry.size {
			t.Fatalf("not a canonical pointer: %q, %v", body, err)
		}
		if err := gitVerifyPayload(context.Background(), f.storage, *pointer); err != nil {
			t.Fatal(err)
		}
		cmd := gitCommand(context.Background(), f.repo, "cat-file", "-e", entry.oid)
		if cmd.Run() == nil {
			t.Fatal("stale oversized object survived repack/prune")
		}
	}
	if len(result.LargeAfter) != 0 {
		t.Fatal("oversized stored objects remain")
	}
	s := mustGitScan(t, f.repo)
	reachable, err := gitReachable(context.Background(), f.repo, s.objects)
	if err != nil || len(reachable) != len(s.objects) {
		t.Fatalf("unreachable objects survived: %v", err)
	}
	mapOnDisk := map[string]string{f.main: f.main, f.hidden: f.hidden, f.remote: f.remote}
	if err := gitReadMap(f.mapPath, mapOnDisk); err != nil || !reflect.DeepEqual(mapOnDisk, result.CommitMap) {
		t.Fatalf("complete persisted map mismatch: %v", err)
	}
	config, _ := os.ReadFile(filepath.Join(f.repo, "config"))
	if bytes.Contains(config, []byte("remote")) {
		t.Fatal("archived config not sanitized")
	}
	verified, err := VerifyLFS(context.Background(), f.repo)
	if err != nil || len(verified) != 2 || !reflect.DeepEqual(verified, result.LFSObjects) {
		t.Fatalf("LFS verification: %v %v", verified, err)
	}
}

func mustGitScan(t *testing.T, repo string) gitState {
	t.Helper()
	s, err := gitScan(context.Background(), repo, 512)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGitRewriteUnreachableCleanup(t *testing.T) {
	gitTestLFS(t)
	for _, kind := range []string{"blob", "commit"} {
		t.Run(kind, func(t *testing.T) {
			f := gitTestFixture(t)
			oid := gitTestBlob(t, f.repo, bytes.Repeat([]byte("x"), 900))
			if kind == "commit" {
				oid = gitTestCommit(t, f.repo, "", map[string]string{"dangling.bin": oid})
			}
			gitTestExec(t, f.repo, nil, "repack", "-ad")
			r, err := RewriteGit(context.Background(), f.repo, 512, f.mapPath)
			if err != nil {
				t.Fatal(err)
			}
			if r.CommitMap[oid] != "" {
				t.Fatal("unreachable object in commit map")
			}
			if gitCommand(context.Background(), f.repo, "cat-file", "-e", oid).Run() == nil {
				t.Fatal("unreachable oversized object survived")
			}
		})
	}
}

func TestGitSafeConfigRemoteMirror(t *testing.T) {
	for _, value := range []string{"true", "false", "TRUE", "FALSE", "invalid", "!command"} {
		t.Run(value, func(t *testing.T) {
			config := "[core]\nrepositoryformatversion = 0\nbare = true\n[remote \"origin\"]\nmirror = " + value + "\n"
			err := gitSafeConfig([]byte(config))
			valid := strings.EqualFold(value, "true") || strings.EqualFold(value, "false")
			if (err == nil) != valid {
				t.Fatalf("mirror value %q: %v", value, err)
			}
		})
	}
}

func TestGitUnsafeArchivesRejected(t *testing.T) {
	cases := map[string]func(*testing.T, gitFixture){
		"remoteCommand": func(t *testing.T, f gitFixture) {
			gitTestAppend(t, filepath.Join(f.repo, "config"), "\n[remote \"origin\"]\nuploadpack = arbitrary-command\n")
		},
		"include": func(t *testing.T, f gitFixture) {
			gitTestAppend(t, filepath.Join(f.repo, "config"), "\n[include]\npath = /tmp/evil\n")
		},
		"filter": func(t *testing.T, f gitFixture) {
			gitTestAppend(t, filepath.Join(f.repo, "config"), "\n[filter \"evil\"]\nclean = arbitrary-command\n")
		},
		"credential": func(t *testing.T, f gitFixture) {
			gitTestAppend(t, filepath.Join(f.repo, "config"), "\n[credential]\nhelper = arbitrary-command\n")
		},
		"sshCommand": func(t *testing.T, f gitFixture) {
			gitTestAppend(t, filepath.Join(f.repo, "config"), "\n[core]\nsshCommand = arbitrary-command\n")
		},
		"lfsURL": func(t *testing.T, f gitFixture) {
			gitTestAppend(t, filepath.Join(f.repo, "config"), "\n[lfs]\nurl = https://evil.invalid\n")
		},
		"alias": func(t *testing.T, f gitFixture) {
			gitTestAppend(t, filepath.Join(f.repo, "config"), "\n[alias]\nlfs = !arbitrary-command\n")
		},
		"bareFalse": func(t *testing.T, f gitFixture) { gitTestExec(t, f.repo, nil, "config", "core.bare", "false") },
		"alternates": func(t *testing.T, f gitFixture) {
			gitTestWrite(t, filepath.Join(f.repo, "objects/info/alternates"), []byte("/tmp/other/objects\n"))
		},
		"shallow": func(t *testing.T, f gitFixture) {
			gitTestWrite(t, filepath.Join(f.repo, "shallow"), []byte(f.main+"\n"))
		},
		"grafts": func(t *testing.T, f gitFixture) {
			gitTestWrite(t, filepath.Join(f.repo, "info/grafts"), []byte(f.main+"\n"))
		},
		"worktree": func(t *testing.T, f gitFixture) {
			gitTestWrite(t, filepath.Join(f.repo, "commondir"), []byte("/tmp/other\n"))
		},
		"promisor": func(t *testing.T, f gitFixture) {
			gitTestWrite(t, filepath.Join(f.repo, "objects/pack/archive.promisor"), nil)
		},
		"replacePacked": func(t *testing.T, f gitFixture) {
			gitTestExec(t, f.repo, nil, "update-ref", "refs/replace/"+f.main, f.hidden)
			gitTestExec(t, f.repo, nil, "pack-refs", "--all", "--prune")
			_ = os.Remove(filepath.Join(f.repo, "refs/replace"))
		},
		"symlink": func(t *testing.T, f gitFixture) {
			if err := os.Symlink(filepath.Join(f.root, "elsewhere"), filepath.Join(f.repo, "objects/symlink")); err != nil {
				t.Fatal(err)
			}
		},
		"detachedHEAD": func(t *testing.T, f gitFixture) { gitTestWrite(t, filepath.Join(f.repo, "HEAD"), []byte(f.main+"\n")) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := gitTestFixture(t)
			mutate(t, f)
			if _, err := InspectGit(context.Background(), f.repo, 512); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func gitTestWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
}

func gitTestAppend(t *testing.T, path, text string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gitTestWrite(t, path, append(b, text...))
}

func TestGitNoncommitRefsAndSignedTagsFailClosed(t *testing.T) {
	for _, kind := range []string{"blob", "tree", "tag-to-blob", "signed-tag"} {
		t.Run(kind, func(t *testing.T) {
			f := gitTestFixture(t)
			oid := f.big
			switch kind {
			case "tree":
				oid = gitTestExec(t, f.repo, nil, "rev-parse", f.main+"^{tree}")
			case "tag-to-blob":
				oid = gitTestExec(t, f.repo, []byte("object "+f.big+"\ntype blob\ntag blob-tag\ntagger A <a@example.invalid> 1577836800 +0000\n\nblob\n"), "hash-object", "-t", "tag", "-w", "--stdin")
			case "signed-tag":
				oid = gitTestExec(t, f.repo, []byte("object "+f.hidden+"\ntype commit\ntag signed\ntagger A <a@example.invalid> 1577836800 +0000\n\nsigned\n-----BEGIN PGP SIGNATURE-----\nnot-a-real-signature\n"), "hash-object", "-t", "tag", "-w", "--stdin")
			}
			gitTestExec(t, f.repo, nil, "update-ref", "refs/custom/unsupported", oid)
			before := mustGitScan(t, f.repo)
			if _, err := RewriteGit(context.Background(), f.repo, 512, f.mapPath); err == nil {
				t.Fatal("unsupported ref accepted")
			}
			after := mustGitScan(t, f.repo)
			if !reflect.DeepEqual(before.Refs, after.Refs) {
				t.Fatal("refs changed on preflight failure")
			}
		})
	}
}

func TestGitSignedObjectsNoop(t *testing.T) {
	for _, kind := range []string{"gpgsig", "gpgsig-sha256", "signed-tag"} {
		for _, conversion := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/conversion=%t", kind, conversion), func(t *testing.T) {
				fixture := gitTestFixture(t)
				objectType := "commit"
				body := gitTestExec(t, fixture.repo, nil, "cat-file", "-p", fixture.main) + "\n"
				if kind == "signed-tag" {
					objectType = "tag"
					body = "object " + fixture.main + "\ntype commit\ntag signed\ntagger A <a@example.invalid> 1577836800 +0000\n\nsigned\n-----BEGIN PGP SIGNATURE-----\nnot-a-real-signature\n"
				} else {
					body = strings.Replace(body, "\n\n", "\n"+kind+" -----BEGIN SSH SIGNATURE-----\n not-a-real-signature\n -----END SSH SIGNATURE-----\n\n", 1)
				}
				objectID := gitTestExec(t, fixture.repo, []byte(body), "hash-object", "-t", objectType, "-w", "--stdin")
				gitTestExec(t, fixture.repo, nil, "update-ref", "refs/custom/signed", objectID)
				before := mustGitScan(t, fixture.repo)
				threshold := DefaultThreshold
				if conversion {
					threshold = 512
				}
				report, err := RewriteGit(context.Background(), fixture.repo, threshold, fixture.mapPath)
				if conversion {
					if err == nil || !strings.Contains(err.Error(), "signature policy") {
						t.Fatalf("signed rewrite was not rejected: %v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					for oldID, newID := range report.CommitMap {
						if oldID != newID {
							t.Fatal("no-op changed commit identity")
						}
					}
				}
				if !reflect.DeepEqual(before.Refs, mustGitScan(t, fixture.repo).Refs) {
					t.Fatal("signed object refs changed")
				}
				if gitTestExec(t, fixture.repo, nil, "cat-file", "-p", objectID) != strings.TrimSpace(body) {
					t.Fatal("signed object content changed")
				}
			})
		}
	}
}

func TestGitIsolationAndCancellation(t *testing.T) {
	gitTestLFS(t)
	f := gitTestFixture(t)
	global := filepath.Join(f.root, "global-config")
	gitTestWrite(t, global, []byte("[include]\npath = /nonexistent/config\n[core]\nsshCommand = false\n[filter \"lfs\"]\nprocess = false\n"))
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
	t.Setenv("GIT_CONFIG_VALUE_0", "false")
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", "/nonexistent/objects")
	t.Setenv("GIT_DIR", "/nonexistent/repository")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InspectGit(ctx, f.repo, 512); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := RewriteGit(context.Background(), f.repo, 512, f.mapPath); err != nil {
		t.Fatal(err)
	}
}

func TestGitRewriteNoopAndExternalPathGuards(t *testing.T) {
	f := gitTestFixture(t)
	if _, err := RewriteGit(context.Background(), f.repo, 1e9, filepath.Join(f.repo, "map.csv")); err == nil {
		t.Fatal("internal object map accepted")
	}
	gitTestWrite(t, f.mapPath, []byte("do not overwrite"))
	if _, err := RewriteGit(context.Background(), f.repo, 1e9, f.mapPath); err == nil {
		t.Fatal("existing object map overwritten")
	}
	if err := os.Remove(f.mapPath); err != nil {
		t.Fatal(err)
	}
	result, err := RewriteGit(context.Background(), f.repo, 1e9, f.mapPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.RefsBefore, result.RefsAfter) || len(result.CommitMap) != 3 {
		t.Fatal("no-op changed refs or omitted identity mappings")
	}
	for old, newOID := range result.CommitMap {
		if old != newOID {
			t.Fatal("no-op rewrote a commit")
		}
	}
}

func TestGitExistingLFSPayloadIntegrity(t *testing.T) {
	gitTestLFS(t)
	f := gitTestFixture(t)
	payload := []byte("existing payload only in a hidden ref")
	hash := sha256.Sum256(payload)
	oid := hex.EncodeToString(hash[:])
	pointerBody := []byte(fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, len(payload)))
	pointerOID := gitTestBlob(t, f.repo, pointerBody)
	// Archived endpoints must never be used by offline verification or migration.
	lfsConfig := gitTestBlob(t, f.repo, []byte("[lfs]\nurl = https://do-not-contact.invalid/lfs\npushurl = https://do-not-contact.invalid/lfs\n"))
	commit := gitTestCommit(t, f.repo, f.main, map[string]string{"existing.bin": pointerOID, ".lfsconfig": lfsConfig})
	commit = gitTestCommit(t, f.repo, commit, nil) // Pointer exists only in history.
	gitTestExec(t, f.repo, nil, "update-ref", "refs/pull/999/head", commit)
	before := mustGitScan(t, f.repo)
	if _, err := RewriteGit(context.Background(), f.repo, 512, f.mapPath); err == nil || !strings.Contains(err.Error(), "fetch separately") {
		t.Fatalf("missing payload not blocked: %v", err)
	}

	if _, err := VerifyLFS(context.Background(), f.repo); err == nil {
		t.Fatal("missing payload accepted")
	}
	after := mustGitScan(t, f.repo)
	if !reflect.DeepEqual(before.Refs, after.Refs) {
		t.Fatal("preflight changed Git refs")
	}
	payloadPath := filepath.Join(f.storage, "objects", oid[:2], oid[2:4], oid)
	gitTestWrite(t, payloadPath, []byte("short"))
	if _, err := VerifyLFS(context.Background(), f.repo); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("wrong length accepted: %v", err)
	}
	gitTestWrite(t, payloadPath, bytes.Repeat([]byte("!"), len(payload)))
	if _, err := VerifyLFS(context.Background(), f.repo); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("corrupt payload accepted: %v", err)
	}
	if _, err := RewriteGit(context.Background(), f.repo, 512, f.mapPath); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("corrupt rewrite accepted: %v", err)
	}
	gitTestWrite(t, payloadPath, payload)
	if _, err := RewriteGit(context.Background(), f.repo, 512, f.mapPath); err != nil {
		t.Fatal(err)
	}
}

func TestGitPointerValidationAndSanitizedErrors(t *testing.T) {
	for _, body := range []string{
		"version https://git-lfs.github.com/spec/v1\noid sha256:bad\nsize 1\n",
		"version https://git-lfs.github.com/spec/v1\next-0-foo value\noid sha256:" + strings.Repeat("a", 64) + "\nsize 1\n",
		"version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize -1\n",
	} {
		if _, err := gitParsePointer([]byte(body)); err == nil {
			t.Fatalf("invalid pointer accepted: %q", body)
		}
	}
	if p, err := gitParsePointer([]byte("ordinary file")); err != nil || p != nil {
		t.Fatal("ordinary file treated as pointer")
	}
	_, repo := gitTestRepo(t)
	err := gitRun(context.Background(), repo, nil, &bytes.Buffer{}, "not-a-command-secret-token")
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("unsafe command error: %v", err)
	}
}

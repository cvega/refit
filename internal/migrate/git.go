package migrate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultThreshold is the customer's 1 GB feature-flag limit, in bytes.
const DefaultThreshold int64 = 1_000_000_000

// Blob describes a stored Git blob, not an LFS payload.
type Blob struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type GitReport struct {
	RefsBefore  map[string]string `json:"refs_before"`
	RefsAfter   map[string]string `json:"refs_after"`
	LargeBefore []Blob            `json:"large_before"`
	LargeAfter  []Blob            `json:"large_after"`
	CommitMap   map[string]string `json:"commit_map"`
	LFSObjects  []string          `json:"lfs_objects"`
}

const gitAliasPrefix = "refs/heads/refit-"
const gitPointerLimit = 1024

type gitObject struct {
	OID, Kind string
	Size      int64
}

type gitState struct {
	Refs       map[string]string
	LargeBlobs []Blob
	objects    []gitObject
}

// gitEnv discards caller Git overrides (including alternates, tracing, injected
// config and repository paths). No operation reads system or global config.
// The installed Git/Git LFS executables and PATH are trusted.
func gitEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "GIT_") && !strings.HasPrefix(key, "LFS_") {
			env = append(env, entry)
		}
	}
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_ATTR_NOSYSTEM=1", "GIT_ALLOW_PROTOCOL=", "LC_ALL=C")
	return env
}

func gitCommand(ctx context.Context, repo string, args ...string) *exec.Cmd {
	base := []string{"--git-dir=" + repo, "-c", "core.bare=true", "-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false", "-c", "core.attributesFile=" + os.DevNull,
		"-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "gc.reflogExpire=never",
		"-c", "lfs.basictransfersonly=true", "-c", "lfs.standalonetransferagent=",
		"-c", "lfs.allowincompletepush=false", "-c", "lfs.fetchinclude=", "-c", "lfs.fetchexclude="}
	base = append(base, "-c", "lfs.storage="+filepath.Join(repo, "lfs"), "-c", "protocol.allow=never")
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Dir = repo
	cmd.Env = gitEnv()
	// Never return command arguments, stderr, URLs, helper output or environment.
	cmd.Stderr = io.Discard
	return cmd
}

func gitFailure(ctx context.Context, operation string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("Git %s failed (command output suppressed)", operation)
}

func gitRun(ctx context.Context, repo string, input io.Reader, output io.Writer, args ...string) error {
	cmd := gitCommand(ctx, repo, args...)
	cmd.Stdin, cmd.Stdout = input, output
	if err := cmd.Run(); err != nil {
		return gitFailure(ctx, "local operation")
	}
	return nil
}

// gitLines consumes metadata incrementally; subprocess errors never expose output.
func gitLines(ctx context.Context, repo string, consume func(string) error, args ...string) error {
	cmd := gitCommand(ctx, repo, args...)
	pipe, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return gitFailure(ctx, "metadata scan")
	}
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		if err = consume(scanner.Text()); err != nil {
			break
		}
	}
	if err == nil {
		err = scanner.Err()
	}
	if err != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if err != nil {
		return err
	}
	if waitErr != nil {
		return gitFailure(ctx, "metadata scan")
	}
	return nil
}

func gitOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// gitPath checks every existing component, including parents outside the repo.
// Callers must exclusively own the disposable extraction and external storage:
// these checks do not defend against concurrent filesystem substitution.
func gitPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("Git paths must be clean absolute paths")
	}
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !os.IsNotExist(err) {
			return errors.New("cannot inspect Git path")
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlinks are unsupported in Git paths")
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}

func gitInside(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func gitReadSmall(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read Git control file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("unsupported Git control file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("cannot read Git control file")
	}
	return b, nil
}

// gitSafeConfig deliberately accepts a small, auditable config grammar, rather
// than asking Git to parse an untrusted config first. Includes, aliases, filters,
// credentials, extensions, worktrees and LFS settings are not accepted. Ordinary
// remote URL/refspec declarations are inert during isolated local operations.
func gitSafeConfig(data []byte) error {
	section, bare, format := "", false, false
	seen := make(map[string]bool)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if strings.EqualFold(line, "[core]") {
				section = "core"
				continue
			}
			if strings.EqualFold(line, "[lfs]") {
				section = "lfs"
				continue
			}
			if strings.HasPrefix(line, "[remote \"") && strings.HasSuffix(line, "\"]") {
				name := strings.TrimSuffix(strings.TrimPrefix(line, "[remote \""), "\"]")
				if name != "" && strings.IndexFunc(name, func(c rune) bool {
					return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.", c))
				}) == -1 {
					section = "remote"
					continue
				}
			}
			return errors.New("unsupported or unsafe archived Git config section")
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
		if !ok || value == "" || strings.ContainsAny(value, "\\\"\x00\r") {
			return errors.New("unsupported archived Git config syntax")
		}
		if section == "remote" && (key == "url" || key == "fetch" || key == "pushurl") {
			continue
		}
		if section == "remote" && key == "mirror" && (strings.EqualFold(value, "true") || strings.EqualFold(value, "false")) {
			continue
		}
		// git-lfs writes this inert format marker on first use, even for fetch.
		if section == "lfs" && key == "repositoryformatversion" && value == "0" {
			continue
		}
		if section != "core" || seen[key] {
			return errors.New("unsupported archived Git config key")
		}
		seen[key] = true
		switch key {
		case "repositoryformatversion":
			format = value == "0"
			if !format {
				return errors.New("only SHA-1 files-backend Git repositories are supported")
			}
		case "bare":
			bare = strings.EqualFold(value, "true")
		case "filemode", "ignorecase", "precomposeunicode", "logallrefupdates", "symlinks":
			if !strings.EqualFold(value, "true") && !strings.EqualFold(value, "false") {
				return errors.New("unsupported archived Git core setting")
			}
		default:
			return errors.New("unsupported or unsafe archived Git core setting")
		}
	}
	if !bare || !format {
		return errors.New("a bare extracted SHA-1 Git repository is required")
	}
	return nil
}

func gitSafety(ctx context.Context, repo string) error {
	if err := gitPath(repo); err != nil {
		return err
	}
	for _, path := range []string{"objects", "refs"} {
		info, err := os.Lstat(filepath.Join(repo, path))
		if err != nil || !info.IsDir() {
			return errors.New("a bare extracted Git repository is required")
		}
	}
	if err := filepath.WalkDir(repo, func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return errors.New("cannot inspect extracted Git repository")
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return errors.New("symlinks and special files are unsupported in extracted Git repositories")
		}
		rel, _ := filepath.Rel(repo, path)
		rel = filepath.ToSlash(rel)
		switch rel {
		case "shallow", "info/grafts", "objects/info/alternates", "objects/info/http-alternates", "commondir", "gitdir", "worktrees", "config.worktree", "refs/replace", "refs/notes":
			return errors.New("unsupported Git layout: alternates, shallow/grafted/replaced history, notes or worktrees")
		}
		return nil
	}); err != nil {
		return err
	}
	config, err := gitReadSmall(filepath.Join(repo, "config"), 64<<10)
	if err != nil {
		return err
	}
	if err := gitSafeConfig(config); err != nil {
		return err
	}
	head, err := gitReadSmall(filepath.Join(repo, "HEAD"), 4096)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(head), "ref: refs/heads/") || strings.ContainsAny(strings.TrimSuffix(string(head), "\n"), "\r\n\x00") {
		return errors.New("bare repository HEAD must name a local branch; detached HEAD is unsupported")
	}
	return nil
}

func gitScan(ctx context.Context, repo string, threshold int64) (gitState, error) {
	s := gitState{Refs: make(map[string]string), LargeBlobs: []Blob{}}
	if threshold <= 0 {
		return s, errors.New("blob threshold must be positive bytes")
	}
	if err := gitSafety(ctx, repo); err != nil {
		return s, err
	}
	caseRefs := make(map[string]bool)
	err := gitLines(ctx, repo, func(line string) error {
		parts := strings.Split(line, "\x00")
		if len(parts) != 3 || !gitOID(parts[1]) || !strings.HasPrefix(parts[0], "refs/") {
			return errors.New("invalid Git ref inventory")
		}
		if strings.HasPrefix(parts[0], "refs/replace/") || strings.HasPrefix(parts[0], "refs/notes/") || strings.HasPrefix(parts[0], gitAliasPrefix) {
			return errors.New("replace, notes or reserved refit refs are unsupported")
		}
		if parts[2] != "" {
			return errors.New("symbolic refs other than HEAD are unsupported")
		}
		folded := strings.ToLower(parts[0])
		if caseRefs[folded] {
			return errors.New("case-colliding Git refs are unsupported")
		}
		caseRefs[folded] = true
		s.Refs[parts[0]] = parts[1]
		return nil
	}, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)")
	if err != nil {
		return s, err
	}
	err = gitLines(ctx, repo, func(line string) error {
		parts := strings.Fields(line)
		if len(parts) != 3 || !gitOID(parts[0]) {
			return errors.New("invalid Git object inventory")
		}
		size, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || size < 0 {
			return errors.New("invalid Git object size")
		}
		s.objects = append(s.objects, gitObject{parts[0], parts[1], size})
		if parts[1] == "blob" && size > threshold {
			s.LargeBlobs = append(s.LargeBlobs, Blob{parts[0], size})
		}
		return nil
	}, "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	return s, err
}

// InspectGit is read-only and scans ALL stored objects, including unreachable
// objects. The caller normally supplies 1_000_000_000 bytes for the customer's
// feature flag. Equality is allowed; no public 400 MiB limit is imposed.
// Only RefsBefore and LargeBefore describe this inspection; other fields are empty.
// Pointer payload verification is a separate, offline VerifyLFS operation.
func InspectGit(ctx context.Context, repo string, threshold int64) (GitReport, error) {
	s, err := gitScan(ctx, repo, threshold)
	return GitReport{RefsBefore: s.Refs, LargeBefore: s.LargeBlobs}, err
}

// gitSmallObjects uses one batch process and bounds each body before reading it.
// Oversized blobs are never buffered, even when the threshold exceeds 1 GB.
func gitSmallObjects(ctx context.Context, repo string, objects []gitObject, visit func(gitObject, []byte) error) error {
	cmd := gitCommand(ctx, repo, "cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		return gitFailure(ctx, "object read")
	}
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		_ = in.Close()
		return gitFailure(ctx, "object read")
	}
	r := bufio.NewReader(out)
	for _, object := range objects {
		limit := int64(gitPointerLimit)
		if object.Kind == "tag" || object.Kind == "commit" {
			limit = 1 << 20
		} else if object.Kind != "blob" || object.Size > limit {
			continue
		}
		if object.Size > limit {
			err = errors.New("commit or annotated tag over 1 MiB is unsupported")
			break
		}
		if _, err = io.WriteString(in, object.OID+"\n"); err != nil {
			break
		}
		var header string
		header, err = r.ReadString('\n')
		if err != nil {
			break
		}
		if header != fmt.Sprintf("%s %s %d\n", object.OID, object.Kind, object.Size) {
			err = errors.New("Git object changed while reading")
			break
		}
		body := make([]byte, int(object.Size)+1)
		if _, err = io.ReadFull(r, body); err != nil {
			break
		}
		if body[len(body)-1] != '\n' {
			err = errors.New("invalid Git batch object framing")
			break
		}
		if err = visit(object, body[:len(body)-1]); err != nil {
			break
		}
	}
	_ = in.Close()
	if err != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if waitErr != nil {
		return gitFailure(ctx, "object read")
	}
	return nil
}

type gitPointer struct {
	OID  string
	Size int64
}

func gitParsePointer(body []byte) (*gitPointer, error) {
	if !bytes.HasPrefix(body, []byte("version https://git-lfs.github.com/spec/")) {
		return nil, nil
	}
	lines := strings.Split(string(body), "\n")
	if len(lines) != 4 || lines[0] != "version https://git-lfs.github.com/spec/v1" || lines[3] != "" || !strings.HasPrefix(lines[1], "oid sha256:") || !strings.HasPrefix(lines[2], "size ") {
		return nil, errors.New("noncanonical or extended LFS pointer is unsupported")
	}
	oid := strings.TrimPrefix(lines[1], "oid sha256:")
	decoded, err := hex.DecodeString(oid)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(oid) != oid {
		return nil, errors.New("invalid LFS pointer hash")
	}
	sizeText := strings.TrimPrefix(lines[2], "size ")
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil || size < 0 || strconv.FormatInt(size, 10) != sizeText {
		return nil, errors.New("invalid LFS pointer size")
	}
	return &gitPointer{oid, size}, nil
}

func gitVerifyPayload(ctx context.Context, storage string, pointer gitPointer) error {
	path := filepath.Join(storage, "objects", pointer.OID[:2], pointer.OID[2:4], pointer.OID)
	if err := gitPath(path); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return errors.New("LFS payload missing; operator must fetch separately into repo/lfs/objects before rewriting (no implicit network access)")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != pointer.Size {
		return errors.New("LFS payload size mismatch")
	}
	hash := sha256.New()
	buf := make([]byte, 128<<10)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := f.Read(buf)
		_, _ = hash.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("cannot read LFS payload")
		}
	}
	if hex.EncodeToString(hash.Sum(nil)) != pointer.OID {
		return errors.New("LFS payload SHA-256 mismatch")
	}
	return nil
}

// VerifyLFS verifies every small historical pointer, including hidden-only and
// unreachable stored pointers, without fetching. Returned SHA-256 OIDs are sorted
// and deduplicated. Extended/noncanonical pointers fail closed.
func VerifyLFS(ctx context.Context, repo string) ([]string, error) {
	s, err := gitScan(ctx, repo, DefaultThreshold)
	if err != nil {
		return nil, err
	}
	return gitVerifyPointers(ctx, repo, s.objects)
}

func gitVerifyPointers(ctx context.Context, repo string, objects []gitObject) ([]string, error) {
	checked := make(map[gitPointer]bool)
	oids := make(map[string]string)
	var blobs []gitObject
	for _, object := range objects {
		if object.Kind == "blob" {
			blobs = append(blobs, object)
		}
	}
	err := gitSmallObjects(ctx, repo, blobs, func(object gitObject, body []byte) error {
		if object.Kind != "blob" {
			return nil
		}
		pointer, err := gitParsePointer(body)
		if err != nil || pointer == nil {
			return err
		}
		if !checked[*pointer] {
			if err := gitVerifyPayload(ctx, filepath.Join(repo, "lfs"), *pointer); err != nil {
				return err
			}
			checked[*pointer] = true
			oids[pointer.OID] = ""
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return gitRefKeys(oids), nil
}

func gitReachable(ctx context.Context, repo string, objects []gitObject) ([]gitObject, error) {
	reachable := make(map[string]bool)
	err := gitLines(ctx, repo, func(line string) error {
		if !gitOID(line) {
			return errors.New("invalid reachable object inventory")
		}
		reachable[line] = true
		return nil
	}, "rev-list", "--objects", "--all", "--no-object-names")
	if err != nil {
		return nil, err
	}
	var result []gitObject
	for _, object := range objects {
		if reachable[object.OID] {
			result = append(result, object)
			delete(reachable, object.OID)
		}
	}
	if len(reachable) != 0 {
		return nil, errors.New("reachable Git objects missing from object inventory")
	}
	return result, nil
}

func gitRefKeys(refs map[string]string) []string {
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func gitUpdateRefs(ctx context.Context, repo, commands string) error {
	return gitRun(ctx, repo, strings.NewReader("start\n"+commands+"prepare\ncommit\n"), io.Discard, "update-ref", "--no-deref", "--stdin")
}

// RewriteGit operates ONLY on an exclusively owned disposable bare extraction.
// Source archives must remain immutable. It is not transactional: on error after
// migration starts, discard the extraction and retry from the archive. The map
// destination must not exist and must be an absolute path outside the repo.
// LFS payloads are read and written exclusively under repo/lfs/objects.
//
// Noncommit refs, notes/replace refs, symbolic non-HEAD refs,
// commit/tag bodies over 1 MiB, detached HEAD, extended LFS pointers and unsafe
// config/layouts fail closed. Unsigned nested tags retain their annotation bytes;
// their OIDs are reported through RefsBefore/RefsAfter, not the commit-only map.
// Signed commits/tags are preserved on no-op runs and rejected when rewriting.
// SHA-1 files-backend repositories only. Unreachable objects and reflogs are
// discarded on this disposable copy. No clone, fetch or push is performed.
// Config is replaced with minimal bare config only after all preflight checks.
// Threshold is strict >, in bytes, normally 1_000_000_000 (not 400 MiB).
func RewriteGit(ctx context.Context, repo string, threshold int64, mapPath string) (GitReport, error) {
	objectMapPath := mapPath
	result := GitReport{CommitMap: make(map[string]string)}
	before, err := gitScan(ctx, repo, threshold)
	if err != nil {
		return result, err
	}
	result.LargeBefore, result.RefsBefore = before.LargeBlobs, before.Refs
	if err := gitPath(objectMapPath); err != nil {
		return result, err
	}
	if gitInside(repo, objectMapPath) || gitInside(objectMapPath, repo) {
		return result, errors.New("object map must be outside the extracted repository")
	}
	if _, err := os.Lstat(objectMapPath); !os.IsNotExist(err) {
		return result, errors.New("object map destination must not exist")
	}
	promisors, err := filepath.Glob(filepath.Join(repo, "objects", "pack", "*.promisor"))
	if err != nil {
		return result, err
	}
	if len(promisors) != 0 {
		return result, errors.New("rewriting promisor-pack repositories is unsupported; a complete archive is required")
	}
	if err := gitRun(ctx, repo, nil, io.Discard, "fsck", "--full", "--no-reflogs"); err != nil {
		return result, err
	}
	reachable, err := gitReachable(ctx, repo, before.objects)
	if err != nil {
		return result, err
	}
	if _, err := gitVerifyPointers(ctx, repo, reachable); err != nil {
		return result, err
	}
	kinds := make(map[string]string)
	migrate := false
	for _, object := range reachable {
		kinds[object.OID] = object.Kind
		if object.Kind == "blob" && object.Size > threshold {
			migrate = true
		}
		if object.Kind == "commit" {
			result.CommitMap[object.OID] = object.OID
		}
	}
	tags := make(map[string][]byte)
	if err := gitSmallObjects(ctx, repo, reachable, func(object gitObject, body []byte) error {
		if migrate && object.Kind == "commit" {
			headers, _, _ := bytes.Cut(body, []byte("\n\n"))
			for _, line := range bytes.Split(headers, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("gpgsig ")) || bytes.HasPrefix(line, []byte("gpgsig-sha256 ")) || bytes.HasPrefix(line, []byte("mergetag ")) {
					return errors.New("signed commits and embedded merge tags require an explicit signature policy before rewriting")
				}
			}
		}
		if object.Kind == "tag" {
			if migrate && (bytes.Contains(body, []byte("-----BEGIN PGP SIGNATURE-----")) || bytes.Contains(body, []byte("-----BEGIN SSH SIGNATURE-----")) || bytes.Contains(body, []byte("-----BEGIN SIGNED MESSAGE-----"))) {
				return errors.New("signed annotated tags require an explicit signature policy before rewriting")
			}
			tags[object.OID] = body
		}
		return nil
	}); err != nil {
		return result, err
	}
	// Validate tag chains and all ref types before making any changes.
	for _, oid := range before.Refs {
		for depth := 0; kinds[oid] == "tag"; depth++ {
			if depth > 64 {
				return result, errors.New("annotated tag nesting is unsupported")
			}
			line, _, _ := bytes.Cut(tags[oid], []byte("\n"))
			oid = strings.TrimPrefix(string(line), "object ")
		}
		if kinds[oid] != "commit" {
			return result, errors.New("noncommit ref target is unsupported; no refs were discarded")
		}
	}
	if err := os.MkdirAll(filepath.Dir(objectMapPath), 0700); err != nil {
		return result, errors.New("cannot create object map directory")
	}
	// git-lfs creates its object map with O_EXCL; do not pre-create it for
	// migration. A no-op still produces the complete identity map below.
	if !migrate {
		mapFile, err := os.OpenFile(objectMapPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return result, errors.New("cannot reserve object map")
		}
		if err := mapFile.Close(); err != nil {
			return result, errors.New("cannot close object map")
		}
	}
	// Rename rather than truncate, so an archived hard link cannot change a
	// config outside the extraction.
	config, err := os.CreateTemp(repo, "refit-config-")
	if err != nil {
		return result, errors.New("cannot create safe Git config")
	}
	defer os.Remove(config.Name())
	_, writeErr := io.WriteString(config, "[core]\n\trepositoryformatversion = 0\n\tbare = true\n\tfilemode = true\n")
	closeErr := config.Close()
	if writeErr != nil || closeErr != nil || os.Rename(config.Name(), filepath.Join(repo, "config")) != nil {
		return result, errors.New("cannot install safe Git config")
	}
	expectedRefs := make(map[string]string)
	for ref, oid := range before.Refs {
		expectedRefs[ref] = oid
	}
	if migrate {
		var create, remove strings.Builder
		// Root EVERY commit, not just branch tips. Alias results provide an
		// independent cross-check for missing/incorrect object-map entries.
		for _, oid := range gitRefKeys(result.CommitMap) {
			fmt.Fprintf(&create, "create %s%s %s\n", gitAliasPrefix, oid, oid)
			fmt.Fprintf(&remove, "delete %s%s\n", gitAliasPrefix, oid)
		}
		if err := gitUpdateRefs(ctx, repo, create.String()); err != nil {
			return result, err
		}
		head, err := gitReadSmall(filepath.Join(repo, "HEAD"), 4096)
		if err != nil {
			return result, err
		}
		headRef := strings.TrimSpace(strings.TrimPrefix(string(head), "ref: "))
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = gitRun(cleanupCtx, repo, nil, io.Discard, "symbolic-ref", "HEAD", headRef)
			_ = gitUpdateRefs(cleanupCtx, repo, remove.String())
		}()
		// Give git-lfs a valid HEAD even for archives with only hidden refs.
		if err := gitRun(ctx, repo, nil, io.Discard, "symbolic-ref", "HEAD", gitAliasPrefix+gitRefKeys(result.CommitMap)[0]); err != nil {
			return result, err
		}
		// Git LFS 3.7 uses an inclusive byte comparison for --above despite
		// its name. Our limit permits equality, so conversion starts at +1.
		// migrate implies an int64 blob size > threshold, making +1 safe.
		if err := gitRun(ctx, repo, nil, io.Discard, "lfs", "migrate", "import", "--everything", "--skip-fetch", "--yes", "--above="+strconv.FormatInt(threshold+1, 10)+"b", "--object-map="+objectMapPath); err != nil {
			return result, err
		}
		if err := gitReadMap(objectMapPath, result.CommitMap); err != nil {
			return result, err
		}
		aliasCount := 0
		if err := gitLines(ctx, repo, func(line string) error {
			parts := strings.Fields(line)
			if len(parts) != 2 || !strings.HasPrefix(parts[0], gitAliasPrefix) {
				return errors.New("invalid migration alias result")
			}
			old := strings.TrimPrefix(parts[0], gitAliasPrefix)
			if result.CommitMap[old] != parts[1] {
				return errors.New("missing or inconsistent commit map entry for rewritten history")
			}
			aliasCount++
			return nil
		}, "for-each-ref", "--format=%(refname) %(objectname)", gitAliasPrefix+"*"); err != nil {
			return result, err
		}
		if aliasCount != len(result.CommitMap) {
			return result, errors.New("migration lost a temporary commit root")
		}
		mapped := make(map[string]string)
		var remap func(string) (string, error)
		remap = func(oid string) (string, error) {
			if commit, ok := result.CommitMap[oid]; ok {
				return commit, nil
			}
			if tag, ok := mapped[oid]; ok {
				return tag, nil
			}
			body := tags[oid]
			line, rest, ok := bytes.Cut(body, []byte("\n"))
			if !ok {
				return "", errors.New("cannot reconstruct annotated tag")
			}
			old := strings.TrimPrefix(string(line), "object ")
			target, err := remap(old)
			if err != nil {
				return "", err
			}
			if target == old {
				mapped[oid] = oid
				return oid, nil
			}
			var output bytes.Buffer
			if err := gitRun(ctx, repo, io.MultiReader(strings.NewReader("object "+target+"\n"), bytes.NewReader(rest)), &output, "hash-object", "-t", "tag", "-w", "--stdin"); err != nil {
				return "", err
			}
			newOID := strings.TrimSpace(output.String())
			if !gitOID(newOID) {
				return "", errors.New("invalid rewritten tag OID")
			}
			mapped[oid] = newOID
			return newOID, nil
		}
		var updates strings.Builder
		for _, ref := range gitRefKeys(before.Refs) {
			oid, err := remap(before.Refs[ref])
			if err != nil {
				return result, err
			}
			fmt.Fprintf(&updates, "update %s %s\n", ref, oid)
			expectedRefs[ref] = oid
		}
		if err := gitUpdateRefs(ctx, repo, updates.String()+remove.String()); err != nil {
			return result, err
		}
		if err := gitRun(ctx, repo, nil, io.Discard, "symbolic-ref", "HEAD", headRef); err != nil {
			return result, err
		}
	}
	// Persist a complete map including identity mappings for unchanged commits.
	if err := gitWriteMap(objectMapPath, result.CommitMap); err != nil {
		return result, err
	}
	interim, err := gitScan(ctx, repo, threshold)
	if err != nil {
		return result, err
	}
	if len(interim.Refs) != len(before.Refs) {
		return result, errors.New("migration changed the archived ref set")
	}
	for ref, expected := range expectedRefs {
		if interim.Refs[ref] != expected {
			return result, errors.New("migration lost or incorrectly mapped an archived ref")
		}
	}
	// Prune superseded and unrooted history only on this disposable extraction.
	for _, args := range [][]string{{"reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all"}, {"repack", "-Ad"}, {"prune", "--expire=now"}} {
		if err := gitRun(ctx, repo, nil, io.Discard, args...); err != nil {
			return result, err
		}
	}
	after, err := gitScan(ctx, repo, threshold)
	if err != nil {
		return result, err
	}
	result.LargeAfter, result.RefsAfter = after.LargeBlobs, after.Refs
	if len(after.LargeBlobs) != 0 {
		return result, errors.New("oversized Git blobs remain after rewrite and repack")
	}
	if err := gitRun(ctx, repo, nil, io.Discard, "fsck", "--full", "--no-reflogs"); err != nil {
		return result, err
	}
	result.LFSObjects, err = gitVerifyPointers(ctx, repo, after.objects)
	return result, err
}

func gitReadMap(path string, commits map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("cannot read LFS commit map")
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256), 256)
	seen := make(map[string]bool)
	for scanner.Scan() {
		row := strings.Split(scanner.Text(), ",")
		if len(row) != 2 || !gitOID(row[0]) || !gitOID(row[1]) || commits[row[0]] == "" || seen[row[0]] {
			return errors.New("invalid LFS commit map")
		}
		seen[row[0]] = true
		commits[row[0]] = row[1]
	}
	if scanner.Err() != nil {
		return errors.New("cannot scan LFS commit map")
	}
	return nil
}

func gitWriteMap(path string, commits map[string]string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return errors.New("cannot write complete commit map")
	}
	w := csv.NewWriter(f)
	for _, oid := range gitRefKeys(commits) {
		_ = w.Write([]string{oid, commits[oid]})
	}
	w.Flush()
	closeErr := f.Close()
	if w.Error() != nil || closeErr != nil {
		return errors.New("cannot persist complete commit map")
	}
	return nil
}

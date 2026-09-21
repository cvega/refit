package migrate

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode"
)

// ArchiveLimits bounds the entire decompressed tar (headers and padding included)
// and its logical members, including directory entries. Both limits are required.
type ArchiveLimits struct {
	MaxBytes int64
	MaxFiles int
}

// safeRelativePath accepts slash-separated archive paths, including a leading
// ./ and a directory's trailing slash. It never cleans away traversal. Callers
// that require canonical names (policies, for example) must compare the result
// with the input. The special result "." denotes only the archive root.
func safeRelativePath(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:\x00") {
		return "", fmt.Errorf("unsafe relative path %q", name)
	}
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	name = strings.TrimSuffix(name, "/")
	if name == "" || name == "." {
		return ".", nil
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("unsafe relative path %q", name)
		}
	}
	return name, nil
}

func unsupportedGitMember(name string) bool {
	name = strings.ToLower(name)
	for _, suffix := range []string{".bundle", ".git", ".tar", ".gz", ".tgz", ".zip", ".bz2", ".tbz", ".tbz2", ".xz", ".txz", ".zst", ".tzst", ".7z", ".rar"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// foldArchivePath uses Unicode simple folding rather than locale-dependent
// casing. Normalization aliases are additionally rejected by exclusive mkdir /
// file creation on filesystems (such as macOS) that normalize filenames.
func foldArchivePath(name string) string {
	return strings.Map(func(r rune) rune {
		min := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < min {
				min = next
			}
		}
		return min
	}, name)
}

func registerArchivePath(names map[string]string, rel string) error {
	for name := rel; name != "."; name = path.Dir(name) {
		folded := foldArchivePath(name)
		if previous, exists := names[folded]; exists && previous != name {
			return fmt.Errorf("case-colliding archive paths: %s and %s", previous, name)
		}
		names[folded] = name
	}
	return nil
}

// tarBudget limits ALL decompressed bytes, not just regular-file payloads.
// Thus zero-sized-member, PAX-header, and trailing-padding bombs are bounded too.
type tarBudget struct {
	r         io.Reader
	remaining int64
}

func (b *tarBudget) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.r.Read(probe[:])
		if n != 0 {
			return 0, errors.New("archive exceeds maxBytes decompressed-byte budget")
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.remaining -= int64(n)
	return n, err
}

// tarTail also checks that tar did not silently accept a missing end marker.
type tarTail struct {
	r     io.Reader
	bytes int64
	zeros int64
}

func (r *tarTail) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.bytes += int64(n)
	for _, c := range p[:n] {
		if c == 0 {
			r.zeros++
		} else {
			r.zeros = 0
		}
	}
	return n, err
}

func openRegular(name string) (*os.File, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", name)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return nil, fmt.Errorf("unsupported hard-linked file: %s", name)
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) {
		f.Close()
		return nil, fmt.Errorf("file changed while opening: %s", name)
	}
	return f, nil
}

// visitArchive validates one gzip member containing one tar stream. A negative
// budget is used only when rereading an already-extracted original for repack.
// Physical PAX/GNU extension records are handled by archive/tar; callbacks see
// logical members and their complete headers, not those extension records.
func visitArchive(filename string, budget int64, visit func(*tar.Header, string, *tar.Reader) error) error {
	f, err := openRegular(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	buffer := bufio.NewReader(f)
	gz, err := gzip.NewReader(buffer)
	if err != nil {
		return fmt.Errorf("unsupported archive: expected tar.gz: %w", err)
	}
	defer gz.Close()
	gz.Multistream(false)
	var input io.Reader = gz
	if budget >= 0 {
		input = &tarBudget{r: gz, remaining: budget}
	}
	tail := &tarTail{r: input}
	tr := tar.NewReader(tail)
	seen := make(map[string]bool)
	directories := make(map[string]bool)
	names := make(map[string]string)
	var padding int64
	for {
		before := tail.bytes
		h, err := tr.Next()
		if err == io.EOF {
			if tail.bytes-before != padding+1024 {
				return errors.New("invalid tar archive: missing end marker")
			}
			break
		}
		if err != nil {
			return fmt.Errorf("invalid tar archive: %w", err)
		}
		rel, err := safeRelativePath(h.Name)
		if err != nil {
			return err
		}
		if err := registerArchivePath(names, rel); err != nil {
			return err
		}
		isDir := h.Typeflag == tar.TypeDir
		if !isDir && h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return fmt.Errorf("unsupported tar member type %d: %s", h.Typeflag, h.Name)
		}
		if h.Linkname != "" || (isDir && h.Size != 0) || (!isDir && (rel == "." || strings.HasSuffix(h.Name, "/"))) {
			return fmt.Errorf("unsupported tar header: %s", h.Name)
		}
		for key := range h.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse.") || key == "SCHILY.realsize" || key == "SCHILY.filetype" {
				return fmt.Errorf("unsupported sparse/special tar member: %s", h.Name)
			}
		}
		if !isDir && unsupportedGitMember(rel) {
			return fmt.Errorf("unsupported bundled or nested Git archive: %s; extracted bare repositories are required", h.Name)
		}
		if _, exists := seen[rel]; exists {
			return fmt.Errorf("duplicate archive path: %s", h.Name)
		}
		if !isDir && directories[rel] {
			return fmt.Errorf("file/directory archive collision: %s", h.Name)
		}
		for parent := path.Dir(rel); parent != "."; parent = path.Dir(parent) {
			if dir, exists := seen[parent]; exists && !dir {
				return fmt.Errorf("non-directory archive parent: %s", parent)
			}
			directories[parent] = true
		}
		seen[rel] = isDir
		if isDir {
			directories[rel] = true
		}
		if budget >= 0 && h.Size > budget {
			return errors.New("archive member exceeds maxBytes")
		}
		// Repack may update the header size; input padding still follows the
		// original member's size, not the replacement file's size.
		padding = (512 - h.Size%512) % 512
		if err := visit(h, rel, tr); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return fmt.Errorf("invalid tar member data: %w", err)
		}
	}
	// Next stops at the tar end marker, before checking the gzip checksum.
	// Only zero padding is permitted after that marker; reject appended tar data.
	var block [32 * 1024]byte
	for {
		n, err := tail.Read(block[:])
		for _, b := range block[:n] {
			if b != 0 {
				return errors.New("unsupported data after tar end marker")
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid gzip stream: %w", err)
		}
	}
	if tail.bytes < 1024 || tail.bytes%512 != 0 || tail.zeros < 1024 {
		return errors.New("invalid tar archive: missing end marker or incomplete block")
	}
	if _, err := buffer.ReadByte(); err != io.EOF {
		return errors.New("unsupported trailing data or concatenated gzip archives")
	}
	return nil
}

// ExtractArchive streams one tar.gz into a NEW private directory. MaxBytes
// bounds the entire uncompressed tar, including headers/padding; MaxFiles bounds
// logical members, including directories. Both must be positive.
// Files are created exclusively with mode 0600; directories use 0700. Original
// modes/headers are recovered from archive by RepackArchive, not a manifest.
// Failures remove the newly created tree, never a preexisting destination.
// The destination parent must be trusted, with no concurrent tree mutations.
func ExtractArchive(source, dest string, limits ArchiveLimits) (err error) {
	if limits.MaxBytes <= 0 || limits.MaxFiles <= 0 {
		return errors.New("MaxBytes and MaxFiles must be positive")
	}
	if err = os.Mkdir(dest, 0700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(dest))
		}
	}()
	createdDirs := map[string]bool{".": true}
	makeDirs := func(rel string) error {
		if rel == "." {
			return nil
		}
		parent := ""
		for _, part := range strings.Split(rel, "/") {
			parent = path.Join(parent, part)
			if createdDirs[parent] {
				continue
			}
			// Do not accept an existing directory under a different spelling
			// on case-insensitive or Unicode-normalizing filesystems.
			if err := os.Mkdir(filepath.Join(dest, filepath.FromSlash(parent)), 0700); err != nil {
				return fmt.Errorf("cannot create unique archive directory %s: %w", parent, err)
			}
			createdDirs[parent] = true
		}
		return nil
	}
	count := 0
	err = visitArchive(source, limits.MaxBytes, func(h *tar.Header, rel string, tr *tar.Reader) error {
		if count >= limits.MaxFiles {
			return errors.New("archive exceeds MaxFiles member budget")
		}
		count++
		name := filepath.Join(dest, filepath.FromSlash(rel))
		if h.Typeflag == tar.TypeDir {
			return makeDirs(rel)
		}
		if err := makeDirs(path.Dir(rel)); err != nil {
			return err
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(f, tr)
		return errors.Join(copyErr, f.Close())
	})
	return err
}

// archiveTree rejects filesystem symlinks and special files, including in
// excluded subtrees. Callers must exclusively own the tree during all operations;
// Lstat checks are not a sandbox against concurrent filesystem mutation.
func archiveTree(root string) (map[string]os.FileInfo, error) {
	root = filepath.Clean(root)
	files := make(map[string]os.FileInfo)
	names := make(map[string]string)
	err := filepath.Walk(root, func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported filesystem entry: %s", name)
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && info.Mode().IsRegular() && stat.Nlink != 1 {
			return fmt.Errorf("unsupported hard-linked file: %s", name)
		}
		if name == root && !info.IsDir() {
			return errors.New("archive root must be a directory")
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if safe, err := safeRelativePath(rel); err != nil || safe != rel {
			return fmt.Errorf("unsafe filesystem path: %s", rel)
		}
		if err := registerArchivePath(names, rel); err != nil {
			return err
		}
		files[rel] = info
		return nil
	})
	return files, err
}

func pathWithin(name, prefix string) bool {
	return prefix == "." || name == prefix || strings.HasPrefix(name, prefix+"/")
}

func canonicalLocation(name string) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

// RepackArchive requires the immutable original (previously validated with a
// bounded ExtractArchive) and its disposable, exclusively owned extracted tree.
// Surviving logical members retain their original names, order and headers
// (except Size). Removed paths, including obsolete Git packs, are omitted.
// New files/directories follow in lexical order with their current modes.
// excluded contains canonical slash-relative prefixes matched on boundaries.
// output must not exist and must be outside root. On failure it is removed.
// The gzip wrapper and physical tar extension encoding are not byte-preserved.
func RepackArchive(original, root, output string, excluded []string) (err error) {
	for _, prefix := range excluded {
		if safe, e := safeRelativePath(prefix); e != nil || safe != prefix || prefix == "." {
			return fmt.Errorf("invalid exclusion prefix: %q", prefix)
		}
	}
	omit := func(rel string) bool {
		for _, prefix := range excluded {
			if pathWithin(rel, prefix) {
				return true
			}
		}
		return false
	}
	files, err := archiveTree(root)
	if err != nil {
		return err
	}
	rootPath, err := canonicalLocation(root)
	if err != nil {
		return err
	}
	for _, name := range []string{original, output} {
		location, e := canonicalLocation(name)
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(rootPath, location)
		if e != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return errors.New("original and output archives must be outside the extracted root")
		}
	}
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			err = errors.Join(err, os.Remove(output))
		}
	}()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	defer gz.Close()
	defer tw.Close()
	write := func(h *tar.Header, rel string, info os.FileInfo) error {
		if info.IsDir() {
			return tw.WriteHeader(h)
		}
		if unsupportedGitMember(rel) {
			return fmt.Errorf("unsupported bundled or nested Git archive: %s", rel)
		}
		in, err := openRegular(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		defer in.Close()
		actual, err := in.Stat()
		if err != nil || !os.SameFile(info, actual) || info.Size() != actual.Size() {
			return fmt.Errorf("file changed during repack: %s", rel)
		}
		h.Size = actual.Size()
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err = io.Copy(tw, in)
		return err
	}
	seen := make(map[string]bool)
	err = visitArchive(original, -1, func(h *tar.Header, rel string, _ *tar.Reader) error {
		seen[rel] = true
		info, exists := files[rel]
		if !exists || omit(rel) {
			return nil
		}
		if info.IsDir() != (h.Typeflag == tar.TypeDir) {
			return fmt.Errorf("archive member changed type: %s", rel)
		}
		return write(h, rel, info)
	})
	if err != nil {
		return err
	}
	var added []string
	for rel := range files {
		if rel != "." && !seen[rel] && !omit(rel) {
			added = append(added, rel)
		}
	}
	sort.Strings(added)
	for _, rel := range added {
		info := files[rel]
		h, e := tar.FileInfoHeader(info, "")
		if e != nil {
			return e
		}
		h.Name = rel
		if info.IsDir() {
			h.Name += "/"
		}
		if err = write(h, rel, info); err != nil {
			return err
		}
	}
	if err = tw.Close(); err != nil {
		return err
	}
	if err = gz.Close(); err != nil {
		return err
	}
	return f.Close()
}

func reposInTree(files map[string]os.FileInfo) []string {
	var repos []string
	for rel, info := range files {
		if !info.IsDir() {
			continue
		}
		is := func(child string, dir bool) bool {
			entry, ok := files[path.Join(rel, child)]
			return ok && entry.IsDir() == dir
		}
		if is("HEAD", false) && is("objects", true) && (is("refs", true) || is("packed-refs", false)) {
			repos = append(repos, rel)
		}
	}
	sort.Strings(repos)
	return repos
}

// DiscoverRepositories returns sorted slash-relative bare repository paths
// ("." for root), detected by HEAD, objects, and refs or packed-refs. At least
// one is required. This is layout detection, not Git object integrity checking.
// Initial workflows MUST require len(result) == 1: cross-repository metadata
// mapping and automatic main/wiki selection are deliberately unsupported.
// Worktrees, gitfiles, nested archives, bundles, and overlapping repos fail closed.
func DiscoverRepositories(root string) ([]string, error) {
	files, err := archiveTree(root)
	if err != nil {
		return nil, err
	}
	if err := validateRepositoryLayout(root, files); err != nil {
		return nil, err
	}
	repos := reposInTree(files)
	if len(repos) == 0 {
		return nil, errors.New("unsupported archive layout: no bare repositories")
	}
	return repos, nil
}

func validateRepositoryLayout(root string, files map[string]os.FileInfo) error {
	for rel, info := range files {
		if strings.EqualFold(path.Base(rel), ".git") || (rel == "." && strings.EqualFold(filepath.Base(filepath.Clean(root)), ".git")) {
			return fmt.Errorf("unsupported worktree/.git layout: %s", rel)
		}
		if info.Mode().IsRegular() && unsupportedGitMember(rel) {
			return fmt.Errorf("unsupported bundled or nested archive: %s", rel)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		f, err := openRegular(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		var prefix [256]byte
		n, readErr := io.ReadFull(f, prefix[:])
		closeErr := f.Close()
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		text := strings.TrimSpace(string(prefix[:n]))
		if strings.HasPrefix(text, "# v2 git bundle") || strings.HasPrefix(text, "# v3 git bundle") || strings.HasPrefix(text, "gitdir:") {
			return fmt.Errorf("unsupported Git bundle or gitfile: %s", rel)
		}
		if path.Base(rel) == "commondir" {
			if _, ok := files[path.Join(path.Dir(rel), "HEAD")]; ok {
				return fmt.Errorf("unsupported linked worktree: %s", path.Dir(rel))
			}
		}
	}
	repos := reposInTree(files)
	for i, repo := range repos {
		for _, other := range repos[i+1:] {
			if pathWithin(other, repo) {
				return fmt.Errorf("unsupported nested repositories: %s and %s", repo, other)
			}
		}
	}
	return nil
}

// FileSHA256 hashes a regular file without buffering its contents.
func FileSHA256(filename string) (string, error) {
	f, err := openRegular(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SHA256File is retained for existing callers; prefer FileSHA256.
func SHA256File(filename string) (string, error) { return FileSHA256(filename) }

// DiscoverRepos is retained for existing callers; prefer DiscoverRepositories.
func DiscoverRepos(root string) ([]string, error) { return DiscoverRepositories(root) }

// PackArchive streams a private, deterministic tar.gz in lexical path order.
// The output must be new and outside root (including symlinked parent aliases).
// Only directories and regular, singly-linked files are accepted. The caller
// must exclusively own the tree and destination parent throughout the operation.
// Headers use fixed private modes and zero timestamps/ownership, not source
// filesystem attributes. Use RepackArchive if original tar headers are required.
func PackArchive(root, dest string) (err error) {
	files, err := archiveTree(root)
	if err != nil {
		return err
	}
	rootPath, err := canonicalLocation(root)
	if err != nil {
		return err
	}
	outputPath, err := canonicalLocation(dest)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootPath, outputPath)
	if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return errors.New("output archive must be outside root")
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			err = errors.Join(err, os.Remove(dest))
		}
	}()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	defer gz.Close()
	defer tw.Close()
	names := make([]string, 0, len(files))
	for rel := range files {
		names = append(names, rel)
	}
	sort.Strings(names)
	for _, rel := range names {
		info := files[rel]
		h := &tar.Header{Name: rel, Mode: 0600, Typeflag: tar.TypeReg, Size: info.Size(), Format: tar.FormatPAX}
		if info.IsDir() {
			h.Name += "/"
			h.Mode, h.Typeflag, h.Size = 0700, tar.TypeDir, 0
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			continue
		}
		if unsupportedGitMember(rel) {
			return fmt.Errorf("unsupported bundled or nested archive: %s", rel)
		}
		in, err := openRegular(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		actual, statErr := in.Stat()
		if statErr != nil || !os.SameFile(info, actual) || actual.Size() != info.Size() {
			in.Close()
			return fmt.Errorf("file changed during packing: %s", rel)
		}
		headerErr := tw.WriteHeader(h)
		if headerErr != nil {
			in.Close()
			return headerErr
		}
		_, copyErr := io.Copy(tw, in)
		if err := errors.Join(copyErr, in.Close()); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Close()
}

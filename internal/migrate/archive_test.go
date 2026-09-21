package migrate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

type archiveTestMember struct {
	header tar.Header
	body   string
}

func archiveTestFile(name, body string) archiveTestMember {
	return archiveTestMember{header: tar.Header{Name: name, Mode: 0644, Typeflag: tar.TypeReg, Size: int64(len(body))}, body: body}
}

func archiveTestDir(name string) archiveTestMember {
	return archiveTestMember{header: tar.Header{Name: name, Mode: 0755, Typeflag: tar.TypeDir}}
}

func archiveTestTar(t *testing.T, members []archiveTestMember) []byte {
	t.Helper()
	var data bytes.Buffer
	tw := tar.NewWriter(&data)
	for _, member := range members {
		h := member.header
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, member.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func archiveTestGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gw := gzip.NewWriter(&compressed)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func archiveTestWrite(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func archiveTestRead(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func archiveTestArchive(t *testing.T, members []archiveTestMember) (string, int64) {
	t.Helper()
	data := archiveTestTar(t, members)
	name := filepath.Join(t.TempDir(), "input.tar.gz")
	archiveTestWrite(t, name, archiveTestGzip(t, data))
	return name, int64(len(data))
}

func TestSafeRelativePath(t *testing.T) {
	for input, want := range map[string]string{"repo/HEAD": "repo/HEAD", "./repo/": "repo", "./": ".", ".": ".", "a b/c": "a b/c"} {
		got, err := safeRelativePath(input)
		if err != nil || got != want {
			t.Errorf("safeRelativePath(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "/root", "../escape", "a/../b", "a//b", "a/./b", `a\b`, "C:/escape", "a\x00b", "./../escape"} {
		if _, err := safeRelativePath(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestExtractArchivePrivateAndExclusive(t *testing.T) {
	archive, budget := archiveTestArchive(t, []archiveTestMember{
		archiveTestDir("./"), archiveTestFile("./repo/HEAD", "ref: refs/heads/main\n"), archiveTestDir("./repo/"),
	})
	parent := t.TempDir()
	dest := filepath.Join(parent, "tree")
	if err := ExtractArchive(archive, dest, ArchiveLimits{MaxBytes: budget, MaxFiles: 3}); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{".": 0700, "repo": 0700, "repo/HEAD": 0600} {
		info, err := os.Stat(filepath.Join(dest, name))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("mode for %s: %v, %v", name, info, err)
		}
	}
	if err := ExtractArchive(archive, dest, ArchiveLimits{MaxBytes: budget, MaxFiles: 3}); err == nil {
		t.Fatal("reused existing destination")
	}
	if got := string(archiveTestRead(t, filepath.Join(dest, "repo/HEAD"))); got != "ref: refs/heads/main\n" {
		t.Fatalf("existing tree changed: %q", got)
	}
}

func TestExtractArchiveRejectsUnsafeMembers(t *testing.T) {
	cases := map[string][]archiveTestMember{
		"traversal":    {archiveTestFile("../escape", "bad")},
		"inner-parent": {archiveTestFile("a/../escape", "bad")},
		"absolute":     {archiveTestFile("/escape", "bad")},
		"backslash":    {archiveTestFile(`a\escape`, "bad")},
		"duplicate":    {archiveTestFile("file", "first"), archiveTestFile("./file", "last")},
		"case-file":    {archiveTestFile("file", "first"), archiveTestFile("FILE", "last")},
		"case-parent":  {archiveTestFile("dir/a", "first"), archiveTestFile("DIR/b", "last")},
		"unicode-case": {archiveTestFile("K/a", "first"), archiveTestFile("K/b", "last")},
		"dup-dir":      {archiveTestDir("dir/"), archiveTestDir("./dir")},
		"parent-file":  {archiveTestFile("a", ""), archiveTestFile("a/b", "")},
		"implicit-dir": {archiveTestFile("a/b", ""), archiveTestFile("a", "")},
		"root-file":    {archiveTestFile(".", "")},
		"bundle":       {archiveTestFile("repos/main.bundle", "# v2 git bundle\n")},
		"git-file":     {archiveTestFile("repos/main.git", "opaque tar data")},
		"nested-git":   {archiveTestFile("repos/main.git.tar.gz", "opaque gzip data")},
	}
	for label, flag := range map[string]byte{"symlink": tar.TypeSymlink, "hardlink": tar.TypeLink, "fifo": tar.TypeFifo, "device": tar.TypeChar} {
		member := archiveTestMember{header: tar.Header{Name: "bad", Typeflag: flag, Mode: 0600}}
		if flag == tar.TypeSymlink || flag == tar.TypeLink {
			member.header.Linkname = "../outside"
		}
		cases[label] = []archiveTestMember{member}
	}
	for label, members := range cases {
		t.Run(label, func(t *testing.T) {
			archive, budget := archiveTestArchive(t, members)
			parent := t.TempDir()
			dest := filepath.Join(parent, "tree")
			if err := ExtractArchive(archive, dest, ArchiveLimits{MaxBytes: budget, MaxFiles: 100}); err == nil {
				t.Fatal("accepted unsafe archive")
			}
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatalf("failed extraction left a tree: %v", err)
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatalf("extraction escaped root: %v %v", entries, err)
			}
		})
	}
}

func TestExtractArchiveBudgetsAndCorruption(t *testing.T) {
	base := archiveTestTar(t, []archiveTestMember{archiveTestFile("file", strings.Repeat("\x00", 2<<20))})
	valid := archiveTestGzip(t, base)
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-8] ^= 0xff
	var many []archiveTestMember
	for i := 0; i < 100; i++ {
		many = append(many, archiveTestFile(strings.Repeat("x", i+1), ""))
	}
	cases := []struct {
		name   string
		data   []byte
		budget int64
	}{
		{"payload-bomb", valid, 1 << 20},
		{"exact-minus-one", valid, int64(len(base) - 1)},
		{"headers-bomb", archiveTestGzip(t, archiveTestTar(t, many)), 4096},
		{"padding-bomb", archiveTestGzip(t, append(bytes.Clone(base), make([]byte, 1<<20)...)), int64(len(base))},
		{"checksum", corrupt, int64(len(base))},
		{"truncated-gzip", valid[:len(valid)-3], int64(len(base))},
		{"truncated-tar", archiveTestGzip(t, base[:len(base)-1024]), int64(len(base))},
		{"one-end-block", archiveTestGzip(t, base[:len(base)-512]), int64(len(base))},
		{"concatenated", append(bytes.Clone(valid), valid...), int64(len(base) * 2)},
		{"trailing-tar", archiveTestGzip(t, append(bytes.Clone(base), base...)), int64(len(base) * 2)},
		{"not-gzip", base, int64(len(base))},
		{"not-tar", archiveTestGzip(t, []byte("not a tar")), 4096},
		{"empty-gzip", archiveTestGzip(t, nil), 4096},
		{"zero-budget", valid, 0},
		{"negative-budget", valid, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			archive := filepath.Join(parent, "bad.tar.gz")
			archiveTestWrite(t, archive, tc.data)
			dest := filepath.Join(parent, "tree")
			if err := ExtractArchive(archive, dest, ArchiveLimits{MaxBytes: tc.budget, MaxFiles: 100}); err == nil {
				t.Fatal("accepted corrupt or over-budget archive")
			}
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatalf("left extraction artifacts: %v", err)
			}
		})
	}
	archive := filepath.Join(t.TempDir(), "good.tar.gz")
	archiveTestWrite(t, archive, valid)
	if err := ExtractArchive(archive, filepath.Join(t.TempDir(), "tree"), ArchiveLimits{MaxBytes: int64(len(base)), MaxFiles: 1}); err != nil {
		t.Fatalf("exact budget failed: %v", err)
	}
}

func TestExtractArchiveRejectsFilesystemDirectoryAliases(t *testing.T) {
	archive, budget := archiveTestArchive(t, []archiveTestMember{
		archiveTestDir("lower/"), archiveTestFile("LOWER/file", "ambiguous parent"),
	})
	dest := filepath.Join(t.TempDir(), "tree")
	err := ExtractArchive(archive, dest, ArchiveLimits{MaxBytes: budget, MaxFiles: 2})
	if err == nil {
		t.Fatal("merged directories with different archive names")
	}
}

func TestRepackArchivePreservesHeadersAndChanges(t *testing.T) {
	old := archiveTestFile("./repos/main.git/HEAD", "old")
	old.header.Mode = 0751
	old.header.Uid, old.header.Gid = 123, 456
	old.header.Uname, old.header.Gname = "owner", "group"
	old.header.ModTime = time.Unix(1700000000, 123456789)
	old.header.AccessTime = time.Unix(1700000010, 0)
	old.header.ChangeTime = time.Unix(1700000020, 0)
	old.header.Format = tar.FormatPAX
	old.header.PAXRecords = map[string]string{"vendor.note": "keep me"}
	old.header.Xattrs = map[string]string{"user.test": "value"}
	members := []archiveTestMember{
		archiveTestDir("./"), archiveTestDir("./repos/"), archiveTestDir("./repos/main.git/"),
		old, archiveTestDir("./repos/main.git/objects/"), archiveTestDir("./repos/main.git/objects/pack/"),
		archiveTestFile("./repos/main.git/objects/pack/old.pack", "old pack"),
		archiveTestFile("./repos/main.git/objects/pack/old.idx", "old index"),
		archiveTestDir("./repos/main.git/lfs/"), archiveTestFile("./repos/main.git/lfs/object", "large"),
		archiveTestFile("./repos/main.git/lfs-other", "keep"), archiveTestFile("./metadata.json", "[]"),
	}
	original, budget := archiveTestArchive(t, members)
	before, err := SHA256File(original)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "tree")
	if err := ExtractArchive(original, root, ArchiveLimits{MaxBytes: budget, MaxFiles: 100}); err != nil {
		t.Fatal(err)
	}
	archiveTestWrite(t, filepath.Join(root, "repos/main.git/HEAD"), []byte("ref: refs/heads/main\n"))
	for _, suffix := range []string{"pack", "idx"} {
		if err := os.Remove(filepath.Join(root, "repos/main.git/objects/pack/old."+suffix)); err != nil {
			t.Fatal(err)
		}
		archiveTestWrite(t, filepath.Join(root, "repos/main.git/objects/pack/new."+suffix), []byte("new "+suffix))
	}
	newName := filepath.Join(root, "new-file")
	archiveTestWrite(t, newName, []byte("added"))
	if err := os.Chmod(newName, 0640); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "repacked.tar.gz")
	if err := RepackArchive(original, root, output, []string{"repos/main.git/lfs"}); err != nil {
		t.Fatal(err)
	}
	after, err := SHA256File(original)
	if err != nil || before != after {
		t.Fatalf("original changed: %s %s %v", before, after, err)
	}
	originalHeaders := make(map[string]tar.Header)
	if err := visitArchive(original, budget, func(h *tar.Header, rel string, _ *tar.Reader) error {
		originalHeaders[rel] = *h
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var names []string
	if err := visitArchive(output, 1<<20, func(h *tar.Header, rel string, tr *tar.Reader) error {
		names = append(names, h.Name)
		if oldHeader, ok := originalHeaders[rel]; ok {
			oldHeader.Size = h.Size
			if !reflect.DeepEqual(oldHeader, *h) {
				t.Errorf("header changed for %s:\n%+v\n%+v", rel, oldHeader, *h)
			}
		}
		if rel == "new-file" && h.Mode != 0640 {
			t.Errorf("new mode = %o", h.Mode)
		}
		if rel == "repos/main.git/HEAD" {
			data, err := io.ReadAll(tr)
			if err != nil || string(data) != "ref: refs/heads/main\n" {
				t.Errorf("rewritten HEAD = %q, %v", data, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"./", "./repos/", "./repos/main.git/", "./repos/main.git/HEAD", "./repos/main.git/objects/", "./repos/main.git/objects/pack/", "./repos/main.git/lfs-other", "./metadata.json", "new-file", "repos/main.git/objects/pack/new.idx", "repos/main.git/objects/pack/new.pack"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("member order/names = %v; want %v", names, want)
	}
	if err := ExtractArchive(output, filepath.Join(t.TempDir(), "roundtrip"), ArchiveLimits{MaxBytes: 1 << 20, MaxFiles: 100}); err != nil {
		t.Fatal(err)
	}
	if err := RepackArchive(original, root, output, nil); err == nil {
		t.Fatal("overwrote existing output")
	}
}

func TestRepackArchiveRejectsUnsafeTreesAndInputs(t *testing.T) {
	original, budget := archiveTestArchive(t, []archiveTestMember{archiveTestFile("file", "content")})
	for _, kind := range []string{"symlink", "hardlink", "type-change", "inside-output", "missing-original", "invalid-exclusion", "nested-git", "root-symlink"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "tree")
			if err := ExtractArchive(original, root, ArchiveLimits{MaxBytes: budget, MaxFiles: 100}); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(parent, "out.tar.gz")
			input := original
			var excluded []string
			switch kind {
			case "symlink":
				if err := os.Symlink(original, filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
				excluded = []string{"link"}
			case "hardlink":
				if err := os.Link(filepath.Join(root, "file"), filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
			case "type-change":
				if err := os.Remove(filepath.Join(root, "file")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, "file"), 0700); err != nil {
					t.Fatal(err)
				}
			case "inside-output":
				output = filepath.Join(root, "out.tar.gz")
			case "missing-original":
				input = filepath.Join(parent, "missing")
			case "invalid-exclusion":
				excluded = []string{"../file"}
			case "nested-git":
				archiveTestWrite(t, filepath.Join(root, "other.git.tar"), []byte("archive"))
			case "root-symlink":
				alias := filepath.Join(parent, "alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				root = alias + "/"
			}
			if err := RepackArchive(input, root, output, excluded); err == nil {
				t.Fatal("accepted unsafe repack")
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("left partial output: %v", err)
			}
		})
	}
}

func TestRepackArchiveChangedLastMemberSize(t *testing.T) {
	original, budget := archiveTestArchive(t, []archiveTestMember{archiveTestFile("last", "original")})
	root := filepath.Join(t.TempDir(), "tree")
	if err := ExtractArchive(original, root, ArchiveLimits{MaxBytes: budget, MaxFiles: 100}); err != nil {
		t.Fatal(err)
	}
	archiveTestWrite(t, filepath.Join(root, "last"), []byte(strings.Repeat("new data", 1000)))
	output := filepath.Join(t.TempDir(), "output.tar.gz")
	if err := RepackArchive(original, root, output, nil); err != nil {
		t.Fatal(err)
	}
	roundtrip := filepath.Join(t.TempDir(), "roundtrip")
	if err := ExtractArchive(output, roundtrip, ArchiveLimits{MaxBytes: 1 << 20, MaxFiles: 100}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(archiveTestRead(t, filepath.Join(root, "last")), archiveTestRead(t, filepath.Join(roundtrip, "last"))) {
		t.Fatal("last member changed")
	}
}

func TestDiscoverRepos(t *testing.T) {
	root := t.TempDir()
	for _, repo := range []string{"exports/main.git", "exports/main.wiki.git", "other/nested/bare"} {
		archiveTestWrite(t, filepath.Join(root, repo, "HEAD"), []byte("ref: refs/heads/main\n"))
		if err := os.MkdirAll(filepath.Join(root, repo, "objects"), 0700); err != nil {
			t.Fatal(err)
		}
		if repo == "exports/main.wiki.git" {
			archiveTestWrite(t, filepath.Join(root, repo, "packed-refs"), nil)
		} else if err := os.MkdirAll(filepath.Join(root, repo, "refs"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	archiveTestWrite(t, filepath.Join(root, "not-a-repo", "HEAD"), nil)
	got, err := DiscoverRepos(root)
	want := []string{"exports/main.git", "exports/main.wiki.git", "other/nested/bare"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("DiscoverRepos = %v, %v", got, err)
	}
	got, err = DiscoverRepos(filepath.Join(root, "exports/main.git"))
	if err != nil || !reflect.DeepEqual(got, []string{"."}) {
		t.Fatalf("root repo = %v, %v", got, err)
	}
	archiveTestWrite(t, filepath.Join(root, "nested.bundle"), nil)
	if _, err := DiscoverRepos(root); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("bundle discovery = %v", err)
	}
}

func TestSHA256File(t *testing.T) {
	name := filepath.Join(t.TempDir(), "data")
	data := bytes.Repeat([]byte("streaming hash"), 100000)
	archiveTestWrite(t, name, data)
	sum := sha256.Sum256(data)
	got, err := SHA256File(name)
	if err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("SHA256File = %s, %v", got, err)
	}
	if _, err := SHA256File(filepath.Dir(name)); err == nil {
		t.Fatal("hashed a directory")
	}
}

func archiveTestBareRepo(t *testing.T, root, rel string) {
	t.Helper()
	archiveTestWrite(t, filepath.Join(root, rel, "HEAD"), []byte("ref: refs/heads/main\n"))
	for _, dir := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(root, rel, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExtractArchiveFileLimits(t *testing.T) {
	archive, budget := archiveTestArchive(t, []archiveTestMember{archiveTestDir("dir"), archiveTestFile("dir/file", "")})
	for _, maxFiles := range []int{-1, 0, 1} {
		dest := filepath.Join(t.TempDir(), "tree")
		if err := ExtractArchive(archive, dest, ArchiveLimits{MaxBytes: budget, MaxFiles: maxFiles}); err == nil {
			t.Fatalf("accepted MaxFiles %d", maxFiles)
		}
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Fatalf("left failed extraction: %v", err)
		}
	}
}

func TestExtractArchiveSparseHeaders(t *testing.T) {
	// archive/tar intentionally strips GNU.sparse keys when writing. Construct
	// a physical PAX extension to exercise rejection on the read side instead.
	for _, key := range []string{"GNU.sparse.size", "SCHILY.realsize", "SCHILY.filetype"} {
		record := key + "=0\n"
		length := len(record) + 2
		for {
			next := len(record) + len(fmt.Sprint(length)) + 1
			if next == length {
				break
			}
			length = next
		}
		record = fmt.Sprintf("%d %s", length, record)
		raw := archiveTestTar(t, []archiveTestMember{archiveTestFile("pax", record), archiveTestFile("file", "")})
		raw[156] = tar.TypeXHeader
		for i := 148; i < 156; i++ {
			raw[i] = ' '
		}
		checksum := 0
		for _, b := range raw[:512] {
			checksum += int(b)
		}
		copy(raw[148:156], []byte(fmt.Sprintf("%06o\x00 ", checksum)))
		source := filepath.Join(t.TempDir(), "sparse.tar.gz")
		archiveTestWrite(t, source, archiveTestGzip(t, raw))
		dest := filepath.Join(t.TempDir(), "tree")
		if err := ExtractArchive(source, dest, ArchiveLimits{MaxBytes: int64(len(raw)), MaxFiles: 10}); err == nil {
			t.Fatalf("accepted sparse extension %s", key)
		}
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Fatalf("left sparse extraction: %v", err)
		}
	}
}

func TestPackArchiveDeterministicRoundTrip(t *testing.T) {
	source, budget := archiveTestArchive(t, []archiveTestMember{archiveTestFile("z", "last"), archiveTestDir("empty"), archiveTestFile("a/file", "first")})
	before, err := FileSHA256(source)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "tree")
	if err := ExtractArchive(source, root, ArchiveLimits{MaxBytes: budget, MaxFiles: 3}); err != nil {
		t.Fatal(err)
	}
	outputs := []string{filepath.Join(t.TempDir(), "a.tar.gz"), filepath.Join(t.TempDir(), "b.tar.gz")}
	for _, dest := range outputs {
		if err := os.Chtimes(filepath.Join(root, "z"), time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := PackArchive(root, dest); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(archiveTestRead(t, outputs[0]), archiveTestRead(t, outputs[1])) {
		t.Fatal("packing not deterministic")
	}
	var names []string
	if err := visitArchive(outputs[0], 1<<20, func(h *tar.Header, rel string, _ *tar.Reader) error {
		names = append(names, rel)
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Fatal("non-deterministic ownership")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{".", "a", "a/file", "empty", "z"}) {
		t.Fatalf("member ordering: %v", names)
	}
	roundtrip := filepath.Join(t.TempDir(), "roundtrip")
	if err := ExtractArchive(outputs[0], roundtrip, ArchiveLimits{MaxBytes: 1 << 20, MaxFiles: 5}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(archiveTestRead(t, filepath.Join(root, "a/file")), archiveTestRead(t, filepath.Join(roundtrip, "a/file"))) {
		t.Fatal("roundtrip changed contents")
	}
	if err := PackArchive(root, outputs[0]); err == nil {
		t.Fatal("overwrote existing archive")
	}
	after, err := FileSHA256(source)
	if err != nil || before != after {
		t.Fatalf("source archive mutated: %v", err)
	}
}

func TestPackArchiveRejectsUnsafeInputs(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "inside", "parent-alias", "root-alias", "nested-archive"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "tree")
			archiveTestWrite(t, filepath.Join(root, "file"), []byte("content"))
			dest := filepath.Join(parent, "out.tar.gz")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink("file", filepath.Join(root, "link"))
			case "hardlink":
				err = os.Link(filepath.Join(root, "file"), filepath.Join(root, "link"))
			case "fifo":
				err = syscall.Mkfifo(filepath.Join(root, "fifo"), 0600)
			case "inside":
				dest = filepath.Join(root, "out.tar.gz")
			case "parent-alias":
				err = os.Symlink(root, filepath.Join(parent, "alias"))
				dest = filepath.Join(parent, "alias", "out.tar.gz")
			case "root-alias":
				err = os.Symlink(root, filepath.Join(parent, "alias"))
				root = filepath.Join(parent, "alias") + "/"
			case "nested-archive":
				archiveTestWrite(t, filepath.Join(root, "last.zip"), []byte("unsupported"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := PackArchive(root, dest); err == nil {
				t.Fatal("accepted unsafe tree/output")
			}
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatalf("left output artifact: %v", err)
			}
		})
	}
}

func TestDiscoverRepositoriesUnsupportedLayouts(t *testing.T) {
	if _, err := DiscoverRepositories(t.TempDir()); err == nil {
		t.Fatal("accepted archive with no repository")
	}
	for _, kind := range []string{"bundle-v2", "bundle-v3", "extension", "worktree", "gitfile", "linked-worktree", "overlap", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			archiveTestBareRepo(t, root, "valid")
			switch kind {
			case "bundle-v2":
				archiveTestWrite(t, filepath.Join(root, "opaque"), []byte("# v2 git bundle\n"))
			case "bundle-v3":
				archiveTestWrite(t, filepath.Join(root, "opaque"), []byte("# v3 git bundle\n"))
			case "extension":
				archiveTestWrite(t, filepath.Join(root, "nested.TGZ"), nil)
			case "worktree":
				archiveTestBareRepo(t, root, "checkout/.git")
			case "gitfile":
				archiveTestWrite(t, filepath.Join(root, "opaque"), []byte("gitdir: /somewhere\n"))
			case "linked-worktree":
				archiveTestWrite(t, filepath.Join(root, "linked/HEAD"), nil)
				archiveTestWrite(t, filepath.Join(root, "linked/commondir"), []byte("../valid"))
			case "overlap":
				archiveTestBareRepo(t, root, "valid/nested")
			case "symlink":
				if err := os.Symlink("valid", filepath.Join(root, "alias")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := DiscoverRepositories(root); err == nil {
				t.Fatal("accepted unsupported layout")
			}
		})
	}
}

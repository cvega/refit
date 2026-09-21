package migrate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	metadataOldSHA      = "1111111111111111111111111111111111111111"
	metadataNewSHA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	metadataIdentitySHA = "2222222222222222222222222222222222222222"
)

func metadataTestMap() map[string]string {
	return map[string]string{metadataOldSHA: metadataNewSHA, metadataIdentitySHA: metadataIdentitySHA}
}

func metadataTestPolicy(file string, rules ...MetadataRule) MetadataPolicy {
	return MetadataPolicy{Reviewed: true, Files: map[string][]MetadataRule{file: rules}}
}

func TestRewriteMetadataApprovedPointersAndNumbers(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "records.json")
	input := `{"records":[{"sha":"OLD","count":900719925474099312345678901234567890,"decimal":1.234567890123456789e+80},{"sha":"OLD"},{"sha":null},{"sha":""},{"sha":"IDENTITY"}],"a/b":{"~key":"OLD"},"":"OLD","*":"OLD","untouched":"not a sha"}`
	input = strings.ReplaceAll(strings.ReplaceAll(input, "OLD", metadataOldSHA), "IDENTITY", metadataIdentitySHA)
	archiveTestWrite(t, name, []byte(input))
	policy := metadataTestPolicy("records.json",
		MetadataRule{Path: "/records/*/sha", Action: "commit"},
		MetadataRule{Path: "/a~1b/~0key", Action: "commit"},
		MetadataRule{Path: "/", Action: "commit"},
		MetadataRule{Path: "/*", Action: "commit"},
	)
	count, err := RewriteMetadata(root, policy, metadataTestMap())
	if err != nil || count != 5 {
		t.Fatalf("rewrite = %d, %v", count, err)
	}
	data := archiveTestRead(t, name)
	for _, literal := range []string{"900719925474099312345678901234567890", "1.234567890123456789e+80", metadataIdentitySHA} {
		if !bytes.Contains(data, []byte(literal)) {
			t.Errorf("literal changed: %s", literal)
		}
	}
	if bytes.Contains(data, []byte(metadataOldSHA)) || bytes.Count(data, []byte(metadataNewSHA)) != 5 {
		t.Fatalf("stale SHA or wrong replacements: %s", data)
	}
}

func TestRewriteMetadataValidationIsGlobal(t *testing.T) {
	for label, bad := range map[string]string{
		"unknown-field": `{"new_schema":"OLD"}`,
		"narrative":     `{"body":"mentions OLD twice OLD"}`,
		"unmapped":      `{"ref":"` + strings.Repeat("b", 40) + `"}`,
		"url":           `{"url":"https://example.test/commit/OLD"}`,
		"hex-prefix":    `{"value":"ffffOLDaaaa"}`,
		"uppercase":     `{"value":"` + strings.Repeat("A", 40) + `"}`,
		"object-key":    `{"OLD":42}`,
		"numeric":       `{"value":OLD}`,
		"escaped":       `{"value":"\u0031` + metadataOldSHA[1:] + `"}`,
	} {
		t.Run(label, func(t *testing.T) {
			root := t.TempDir()
			first := []byte(`{"sha":"` + metadataOldSHA + `"}`)
			second := []byte(strings.ReplaceAll(bad, "OLD", metadataOldSHA))
			archiveTestWrite(t, filepath.Join(root, "a.json"), first)
			archiveTestWrite(t, filepath.Join(root, "z.json"), second)
			policy := metadataTestPolicy("a.json", MetadataRule{Path: "/sha", Action: "commit"})
			policy.Files["z.json"] = nil
			count, err := RewriteMetadata(root, policy, metadataTestMap())
			if err == nil || count != 0 {
				t.Fatalf("accepted unreviewed reference: %d %v", count, err)
			}
			for name, want := range map[string][]byte{"a.json": first, "z.json": second} {
				if !bytes.Equal(archiveTestRead(t, filepath.Join(root, name)), want) {
					t.Fatalf("changed %s despite validation failure", name)
				}
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 2 {
				t.Fatalf("left staging files: %v %v", entries, err)
			}
		})
	}
}

func TestRewriteMetadataPreserveSubtree(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "records.json")
	input := `{"sha":"OLD","discussion":{"body":"quoted OLD","OLD":{"unmapped":"UNKNOWN"}},"url":"https://example.test/commit/OLD"}`
	input = strings.ReplaceAll(strings.ReplaceAll(input, "OLD", metadataOldSHA), "UNKNOWN", strings.Repeat("b", 40))
	archiveTestWrite(t, name, []byte(input))
	policy := metadataTestPolicy("records.json", MetadataRule{Path: "/sha", Action: "commit"}, MetadataRule{Path: "/discussion", Action: "preserve"}, MetadataRule{Path: "/url", Action: "preserve"})
	count, err := RewriteMetadata(root, policy, metadataTestMap())
	if err != nil || count != 1 {
		t.Fatalf("preserve = %d %v", count, err)
	}
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(archiveTestRead(t, name), &after); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"discussion", "url"} {
		// Object member order is not part of a JSON value. Compare decoded
		// values, retaining exact number literals rather than using float64.
		var b, a any
		beforeDecoder := json.NewDecoder(bytes.NewReader(before[key]))
		beforeDecoder.UseNumber()
		if err := beforeDecoder.Decode(&b); err != nil {
			t.Fatal(err)
		}
		afterDecoder := json.NewDecoder(bytes.NewReader(after[key]))
		afterDecoder.UseNumber()
		if err := afterDecoder.Decode(&a); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(b, a) {
			t.Fatalf("preserved value changed: %s", key)
		}
	}
}

func TestRewriteMetadataReviewedFilesRequired(t *testing.T) {
	for label, policy := range map[string]MetadataPolicy{
		"zero":         {},
		"unreviewed":   {Files: map[string][]MetadataRule{"a.json": {}}},
		"missing-file": {Reviewed: true},
	} {
		t.Run(label, func(t *testing.T) {
			root := t.TempDir()
			archiveTestWrite(t, filepath.Join(root, "a.json"), []byte("{}"))
			if _, err := RewriteMetadata(root, policy, nil); err == nil {
				t.Fatal("accepted unreviewed file")
			}
		})
	}
	root := t.TempDir()
	name := filepath.Join(root, "a.JSON")
	input := []byte(" { \"number\" : 123 } \n")
	archiveTestWrite(t, name, input)
	count, err := RewriteMetadata(root, metadataTestPolicy("a.JSON"), nil)
	if err != nil || count != 0 || !bytes.Equal(input, archiveTestRead(t, name)) {
		t.Fatalf("empty reviewed policy: %d %v", count, err)
	}
	var policy MetadataPolicy
	if err := json.Unmarshal([]byte(`{"reviewed":true,"files":{"a.JSON":[{"path":"/number","action":"preserve"}]}}`), &policy); err != nil {
		t.Fatal(err)
	}
	if !policy.Reviewed || policy.Files["a.JSON"][0].Path != "/number" {
		t.Fatalf("policy JSON tags: %+v", policy)
	}
}

func TestRewriteMetadataRejectsInvalidDocuments(t *testing.T) {
	cases := map[string][]byte{
		"broken.json":    []byte(`{"broken":`),
		"stream.json":    []byte(`{} {}`),
		"duplicate.json": []byte(`{"sha":"` + metadataOldSHA + `","\u0073ha":"hidden"}`),
		"empty.json":     nil,
		"invalid.json":   {0xff},
		"deep.json":      []byte(strings.Repeat("[", 514) + "0" + strings.Repeat("]", 514)),
		"lines.jsonl":    []byte("{}\n{}\n"),
		"lines.ndjson":   []byte("{}\n"),
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			archiveTestWrite(t, filepath.Join(root, name), bad)
			policy := MetadataPolicy{Reviewed: true, Files: map[string][]MetadataRule{}}
			if strings.HasSuffix(name, ".json") {
				policy.Files[name] = []MetadataRule{{Path: "", Action: "preserve"}}
			}
			if _, err := RewriteMetadata(root, policy, metadataTestMap()); err == nil {
				t.Fatal("accepted invalid metadata")
			}
			if !bytes.Equal(bad, archiveTestRead(t, filepath.Join(root, name))) {
				t.Fatal("changed invalid metadata")
			}
		})
	}
	t.Run("oversized", func(t *testing.T) {
		root := t.TempDir()
		f, err := os.Create(filepath.Join(root, "huge.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(maxMetadataJSONBytes + 1); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := RewriteMetadata(root, metadataTestPolicy("huge.json"), nil); err == nil || !strings.Contains(err.Error(), "64 MiB") {
			t.Fatalf("oversize: %v", err)
		}
	})
}

func TestRewriteMetadataNonJSONStreaming(t *testing.T) {
	for _, size := range []int{40, 64} {
		old, next := strings.Repeat("1", size), strings.Repeat("a", size)
		for _, offset := range []int{0, 32767, 32768, 32768 + 62, 65535} {
			root := t.TempDir()
			first := []byte(`{"sha":"` + old + `"}`)
			archiveTestWrite(t, filepath.Join(root, "a.json"), first)
			attachment := append(bytes.Repeat([]byte{'x'}, offset), []byte(old)...)
			attachment = append(attachment, 0xff, 0)
			archiveTestWrite(t, filepath.Join(root, "z.dat"), attachment)
			count, err := RewriteMetadata(root, metadataTestPolicy("a.json", MetadataRule{Path: "/sha", Action: "commit"}), map[string]string{old: next})
			if err == nil || count != 0 {
				t.Fatalf("missed %d-byte SHA at %d", size, offset)
			}
			if !bytes.Equal(first, archiveTestRead(t, filepath.Join(root, "a.json"))) {
				t.Fatal("partial JSON write")
			}
			if !bytes.Equal(attachment, archiveTestRead(t, filepath.Join(root, "z.dat"))) {
				t.Fatal("attachment changed")
			}
		}
	}
	root := t.TempDir()
	archiveTestWrite(t, filepath.Join(root, "attachment"), []byte(metadataIdentitySHA))
	if n, err := RewriteMetadata(root, MetadataPolicy{Reviewed: true}, metadataTestMap()); err != nil || n != 0 {
		t.Fatalf("identity attachment: %d %v", n, err)
	}
}

func TestRewriteMetadataCommitRulesFailClosed(t *testing.T) {
	for label, value := range map[string]string{
		"unmapped":    `"` + strings.Repeat("b", 40) + `"`,
		"abbreviated": `"1111111"`,
		"number":      "123",
		"bool":        "true",
		"array":       "[]",
		"object":      "{}",
		"URL":         `"https://example.test/commit/` + metadataOldSHA + `"`,
	} {
		t.Run(label, func(t *testing.T) {
			root := t.TempDir()
			archiveTestWrite(t, filepath.Join(root, "a.json"), []byte(`{"sha":`+value+`}`))
			if _, err := RewriteMetadata(root, metadataTestPolicy("a.json", MetadataRule{Path: "/sha", Action: "commit"}), metadataTestMap()); err == nil {
				t.Fatal("accepted invalid commit reference")
			}
		})
	}
	for _, value := range []string{"null", `""`, `"` + metadataIdentitySHA + `"`} {
		root := t.TempDir()
		archiveTestWrite(t, filepath.Join(root, "root.json"), []byte(value))
		if n, err := RewriteMetadata(root, metadataTestPolicy("root.json", MetadataRule{Path: "", Action: "commit"}), metadataTestMap()); err != nil || n != 0 {
			t.Fatalf("nullable/identity root: %d %v", n, err)
		}
	}
}

func TestRewriteMetadataPoliciesFailClosed(t *testing.T) {
	for _, file := range []string{"*.json", "../a.json", "./a.json", "a.txt", "/a.json"} {
		if _, err := RewriteMetadata(t.TempDir(), metadataTestPolicy(file), nil); err == nil {
			t.Fatalf("accepted file policy %q", file)
		}
	}
	for label, rules := range map[string][]MetadataRule{
		"bad-pointer":     {{Path: "sha", Action: "commit"}},
		"bad-escape":      {{Path: "/sha~2", Action: "commit"}},
		"trailing-tilde":  {{Path: "/sha~", Action: "commit"}},
		"action":          {{Path: "/sha", Action: "guess"}},
		"duplicate":       {{Path: "/sha", Action: "commit"}, {Path: "/sha", Action: "preserve"}},
		"subtree-overlap": {{Path: "", Action: "preserve"}, {Path: "/sha", Action: "commit"}},
		"wrong-pointer":   {{Path: "/other", Action: "commit"}},
		"object-wildcard": {{Path: "/*", Action: "commit"}},
	} {
		t.Run(label, func(t *testing.T) {
			root := t.TempDir()
			archiveTestWrite(t, filepath.Join(root, "a.json"), []byte(`{"sha":"`+metadataOldSHA+`"}`))
			if _, err := RewriteMetadata(root, metadataTestPolicy("a.json", rules...), metadataTestMap()); err == nil {
				t.Fatal("accepted invalid policy")
			}
		})
	}
	root := t.TempDir()
	archiveTestWrite(t, filepath.Join(root, "a.json"), []byte(`["`+metadataOldSHA+`"]`))
	policy := metadataTestPolicy("a.json", MetadataRule{Path: "/*", Action: "commit"}, MetadataRule{Path: "/0", Action: "commit"})
	if _, err := RewriteMetadata(root, policy, metadataTestMap()); err == nil {
		t.Fatal("accepted overlapping array selectors")
	}
}

func TestRewriteMetadataCommitURLs(t *testing.T) {
	for _, text := range []string{
		"https://EXAMPLE.test:443/a%2fb/" + metadataOldSHA + "/diff?z=2&x=%2f#fragment",
		"https://example.test/" + metadataOldSHA + "/" + metadataIdentitySHA,
		"https://example.test/%31" + metadataOldSHA[1:],
	} {
		root := t.TempDir()
		name := filepath.Join(root, "a.json")
		data, _ := json.Marshal(text)
		archiveTestWrite(t, name, data)
		count, err := RewriteMetadata(root, metadataTestPolicy("a.json", MetadataRule{Path: "", Action: "commit-url"}), metadataTestMap())
		if err != nil || count != 1 {
			t.Fatalf("URL rewrite: %d %v", count, err)
		}
		var actual string
		if err := json.Unmarshal(archiveTestRead(t, name), &actual); err != nil {
			t.Fatal(err)
		}
		want := strings.ReplaceAll(strings.ReplaceAll(text, "%31"+metadataOldSHA[1:], metadataNewSHA), metadataOldSHA, metadataNewSHA)
		if actual != want {
			t.Fatalf("URL altered: got %s want %s", actual, want)
		}
	}
	for _, text := range []string{
		"https://example.test/" + strings.Repeat("b", 40),
		"https://example.test/prefix-" + metadataOldSHA,
		"https://example.test/" + metadataOldSHA + "?sha=" + metadataOldSHA,
		"https://example.test/" + metadataOldSHA + "#" + metadataOldSHA,
		"https://example.test/1111111", "https://example.test", "/commit/" + metadataOldSHA,
		"https://user:secret@example.test/" + metadataOldSHA,
	} {
		root := t.TempDir()
		data, _ := json.Marshal(text)
		archiveTestWrite(t, filepath.Join(root, "a.json"), data)
		if _, err := RewriteMetadata(root, metadataTestPolicy("a.json", MetadataRule{Path: "", Action: "commit-url"}), metadataTestMap()); err == nil {
			t.Fatal("accepted unsafe commit URL")
		}
	}
}

func TestRewriteMetadataGitScopeAndLayout(t *testing.T) {
	root := t.TempDir()
	archiveTestBareRepo(t, root, "repo")
	archiveTestWrite(t, filepath.Join(root, "repo/custom.json"), []byte("not JSON "+metadataOldSHA))
	archiveTestWrite(t, filepath.Join(root, "repo/refs/heads/main"), []byte(metadataOldSHA))
	if n, err := RewriteMetadata(root, MetadataPolicy{Reviewed: true}, metadataTestMap()); err != nil || n != 0 {
		t.Fatalf("scanned Git internals: %d %v", n, err)
	}
	archiveTestBareRepo(t, root, "other")
	if _, err := RewriteMetadata(root, MetadataPolicy{Reviewed: true}, metadataTestMap()); err == nil {
		t.Fatal("accepted multi-repo map")
	}
}

func TestRewriteMetadataInvalidCommitMaps(t *testing.T) {
	for _, mapping := range []map[string]string{
		{"short": metadataNewSHA}, {metadataOldSHA: "bad"}, {metadataOldSHA: strings.Repeat("a", 64)}, {"bad": "bad"}, {strings.Repeat("A", 40): metadataNewSHA},
	} {
		if _, err := RewriteMetadata(t.TempDir(), MetadataPolicy{Reviewed: true}, mapping); err == nil {
			t.Fatal("accepted invalid map")
		}
	}
}

func TestCommitMetadataStagesAndRollsBack(t *testing.T) {
	for _, failStage := range []bool{true, false} {
		root := t.TempDir()
		first := filepath.Join(root, "first.json")
		archiveTestWrite(t, first, []byte("original"))
		second := filepath.Join(root, "directory")
		if failStage {
			second = filepath.Join(root, "missing", "second.json")
		} else if err := os.Mkdir(second, 0700); err != nil {
			t.Fatal(err)
		}
		edits := []metadataEdit{
			{name: first, before: []byte("original"), after: []byte("changed"), mode: 0600},
			{name: second, before: []byte("before"), after: []byte("after"), mode: 0600},
		}
		if err := commitMetadata(edits); err == nil {
			t.Fatal("expected transaction failure")
		}
		if string(archiveTestRead(t, first)) != "original" {
			t.Fatal("partial rewrite after failed transaction")
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".metadata-") {
				t.Errorf("leaked staging file: %s", entry.Name())
			}
		}
	}
}

func TestRewriteMetadataRejectsFilesystemLinks(t *testing.T) {
	for _, hard := range []bool{false, true} {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.json")
		data := []byte(`{"sha":"` + metadataOldSHA + `"}`)
		archiveTestWrite(t, outside, data)
		link := os.Symlink
		if hard {
			link = os.Link
		}
		if err := link(outside, filepath.Join(root, "linked.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := RewriteMetadata(root, metadataTestPolicy("linked.json", MetadataRule{Path: "/sha", Action: "commit"}), metadataTestMap()); err == nil {
			t.Fatal("accepted linked metadata")
		}
		if !bytes.Equal(data, archiveTestRead(t, outside)) {
			t.Fatal("modified external file")
		}
	}
}

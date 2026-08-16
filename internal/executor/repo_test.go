package executor

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestValidateRepoRejectsTraversal(t *testing.T) {
	bad := []string{
		"../escape.py",
		"../../etc/passwd",
		"a/../../b",
		"/absolute.py",
		"/etc/passwd",
		"foo/../../../bar",
		"..",
		".",
		"", // empty
		"back\\slash.py",
		"nul\x00byte.py",
	}
	for _, p := range bad {
		_, err := ValidateRepo([]RepoFile{{Path: p, Data: []byte("x")}})
		if err == nil {
			t.Errorf("path %q was accepted; it must be rejected", p)
		}
	}
}

func TestValidateRepoAcceptsNormalPaths(t *testing.T) {
	files := []RepoFile{
		{Path: "main.py", Data: []byte("print(1)")},
		{Path: "src/util.py", Data: []byte("x = 1")},
		{Path: "./tests/test_x.py", Data: []byte("assert True")}, // ./ normalizes
	}
	out, err := ValidateRepo(files)
	if err != nil {
		t.Fatalf("valid repo rejected: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d files, want 3", len(out))
	}
	// ./ prefix must be cleaned.
	for _, f := range out {
		if strings.HasPrefix(f.Path, "./") || strings.HasPrefix(f.Path, "/") {
			t.Errorf("path not normalized: %q", f.Path)
		}
	}
}

func TestValidateRepoEnforcesLimits(t *testing.T) {
	// Total size cap.
	big := bytes.Repeat([]byte("a"), MaxRepoFileBytes)
	var many []RepoFile
	for i := 0; i < (MaxRepoBytes/MaxRepoFileBytes)+2; i++ {
		many = append(many, RepoFile{Path: pathN(i), Data: big})
	}
	if _, err := ValidateRepo(many); err == nil {
		t.Error("oversize repo accepted")
	}

	// Per-file cap.
	huge := bytes.Repeat([]byte("a"), MaxRepoFileBytes+1)
	if _, err := ValidateRepo([]RepoFile{{Path: "big.bin", Data: huge}}); err == nil {
		t.Error("oversize single file accepted")
	}

	// File-count cap.
	var lots []RepoFile
	for i := 0; i < MaxRepoFiles+1; i++ {
		lots = append(lots, RepoFile{Path: pathN(i), Data: []byte("x")})
	}
	if _, err := ValidateRepo(lots); err == nil {
		t.Error("too-many-files repo accepted")
	}
}

func TestValidateRepoRejectsDuplicates(t *testing.T) {
	files := []RepoFile{
		{Path: "a.py", Data: []byte("1")},
		{Path: "./a.py", Data: []byte("2")}, // same path after cleaning
	}
	if _, err := ValidateRepo(files); err == nil {
		t.Error("duplicate paths accepted")
	}
}

func TestTarRepoStructure(t *testing.T) {
	files, err := ValidateRepo([]RepoFile{
		{Path: "main.py", Data: []byte("print('hi')")},
		{Path: "pkg/mod.py", Data: []byte("x = 1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	buf, err := TarRepo(files)
	if err != nil {
		t.Fatal(err)
	}

	// Entries are plain repo-relative paths (unpacked into the work dir inside
	// the container); none may contain traversal or be absolute.
	tr := tar.NewReader(buf)
	found := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(h.Name, "..") || strings.HasPrefix(h.Name, "/") {
			t.Errorf("unsafe tar entry %q", h.Name)
		}
		found[h.Name] = true
	}
	for _, want := range []string{"main.py", "pkg/mod.py"} {
		if !found[want] {
			t.Errorf("missing tar entry %q", want)
		}
	}
	if !found["pkg/"] {
		t.Error("missing parent dir entry pkg/")
	}
}

func pathN(i int) string {
	return "f" + string(rune('0'+i%10)) + "_" + itoa(i) + ".txt"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

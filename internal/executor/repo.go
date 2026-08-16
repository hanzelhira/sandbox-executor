package executor

import (
	"archive/tar"
	"bytes"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Repo ingestion.
//
// A repo is a set of files copied into a session's writable tmpfs at
// /tmp/work, after which commands run against it. The validation here is the
// security boundary for untrusted uploads — treat it like auth. A malicious
// archive tries path traversal (../../etc/passwd), absolute paths, symlink
// escapes, or sheer size (zip/tar bombs); every one is rejected before a
// single byte reaches the container.

const (
	RepoWorkDir      = "/tmp/work"
	MaxRepoBytes     = 8 << 20 // 8 MiB total — fits the 64m tmpfs with room
	MaxRepoFiles     = 2000
	MaxRepoFileBytes = 2 << 20 // 2 MiB per file
	MaxRepoPathLen   = 255
)

// RepoFile is one file destined for the sandbox, with a repo-relative path
// using forward slashes (e.g. "src/main.py").
type RepoFile struct {
	Path string
	Data []byte
}

// ValidateRepo rejects an unsafe or oversized file set and returns a
// normalized, sorted copy. Returning normalized paths means callers store and
// display exactly what will be written — no surprises between validation and
// use.
func ValidateRepo(files []RepoFile) ([]RepoFile, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("no files")
	}
	if len(files) > MaxRepoFiles {
		return nil, fmt.Errorf("too many files: %d (max %d)", len(files), MaxRepoFiles)
	}

	var total int
	seen := make(map[string]bool, len(files))
	out := make([]RepoFile, 0, len(files))
	for _, f := range files {
		clean, err := cleanRepoPath(f.Path)
		if err != nil {
			return nil, err
		}
		if seen[clean] {
			return nil, fmt.Errorf("duplicate path %q", clean)
		}
		seen[clean] = true

		if len(f.Data) > MaxRepoFileBytes {
			return nil, fmt.Errorf("%s is %d bytes (max %d per file)", clean, len(f.Data), MaxRepoFileBytes)
		}
		total += len(f.Data)
		if total > MaxRepoBytes {
			return nil, fmt.Errorf("repo exceeds %d bytes total", MaxRepoBytes)
		}
		out = append(out, RepoFile{Path: clean, Data: f.Data})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// CleanRepoPath is the exported traversal guard, reused by delete/rename paths.
func CleanRepoPath(p string) (string, error) { return cleanRepoPath(p) }

// cleanRepoPath rejects anything that could escape the work dir and returns the
// path relative to it. This is the traversal guard.
func cleanRepoPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if len(p) > MaxRepoPathLen {
		return "", fmt.Errorf("path too long: %q", p)
	}
	// Backslashes and NUL are never valid; some archives smuggle traversal
	// through Windows separators.
	if strings.ContainsAny(p, "\\\x00") {
		return "", fmt.Errorf("illegal characters in path %q", p)
	}
	if path.IsAbs(p) {
		return "", fmt.Errorf("absolute path not allowed: %q", p)
	}
	// path.Clean collapses ".." and "."; if the result still starts with ".."
	// (or is exactly ".."), it escapes the root.
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes repo root: %q", p)
	}
	if clean == "." {
		return "", fmt.Errorf("path resolves to repo root: %q", p)
	}
	return clean, nil
}

// TarRepo builds a tar stream with entries at plain repo-relative paths
// (main.py, pkg/mod.py). It is unpacked into RepoWorkDir by a process running
// INSIDE the container — CopyToContainer cannot be used, because Docker refuses
// it on a read-only-rootfs container even for the writable tmpfs mount, so we
// write through an exec instead (see dockerSession.Populate). Assumes files
// have passed ValidateRepo.
func TarRepo(files []RepoFile) (*bytes.Buffer, error) {
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)

	dirs := map[string]bool{}
	writeDir := func(d string) error {
		if d == "" || d == "." || dirs[d] {
			return nil
		}
		dirs[d] = true
		return tw.WriteHeader(&tar.Header{
			Name: d + "/", Typeflag: tar.TypeDir, Mode: 0o755,
		})
	}
	for _, f := range files {
		if err := ensureParents(path.Dir(f.Path), writeDir); err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     f.Path,
			Mode:     0o644,
			Size:     int64(len(f.Data)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf, nil
}

// ensureParents walks a path's ancestry root-to-leaf, calling write for each.
func ensureParents(dir string, write func(string) error) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	parts := strings.Split(dir, "/")
	cur := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if cur == "" {
			cur = p
		} else {
			cur += "/" + p
		}
		if err := write(cur); err != nil {
			return err
		}
	}
	return nil
}

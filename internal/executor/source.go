package executor

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
)

// Source is where a repo's files come from. Upload works fully offline and
// changes nothing about the threat model. Git is scaffolded but disabled: it
// needs network egress, which would run on the CONTROL PLANE (never inside the
// network=none runner), and is an explicit opt-in — see docs/repos-plan.md.
type Source interface {
	// Fetch returns the repo's files, already validated.
	Fetch(ctx context.Context) ([]RepoFile, error)
	Describe() string
}

// UploadSource wraps files already received from the browser (folder upload or
// unpacked zip). This is Option C — nothing leaves the machine.
type UploadSource struct {
	Files []RepoFile
	Label string
}

func (u UploadSource) Fetch(ctx context.Context) ([]RepoFile, error) {
	return ValidateRepo(u.Files)
}

func (u UploadSource) Describe() string {
	if u.Label != "" {
		return "upload: " + u.Label
	}
	return "upload"
}

// FilesFromZip unpacks a zip archive into RepoFiles, applying the same guards
// as ValidateRepo plus an uncompressed-size ceiling to defuse zip bombs (a tiny
// archive that expands to gigabytes). The caller still runs ValidateRepo via
// UploadSource.Fetch; this stops the bomb before it is even collected.
func FilesFromZip(data []byte) ([]RepoFile, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}
	var files []RepoFile
	var total int
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		// Skip VCS/junk that would blow the file-count budget.
		if strings.HasPrefix(zf.Name, "__MACOSX/") || strings.Contains(zf.Name, "/.git/") || strings.HasPrefix(zf.Name, ".git/") {
			continue
		}
		if zf.UncompressedSize64 > MaxRepoFileBytes {
			return nil, fmt.Errorf("%s is %d bytes uncompressed (max %d)", zf.Name, zf.UncompressedSize64, MaxRepoFileBytes)
		}
		total += int(zf.UncompressedSize64)
		if total > MaxRepoBytes {
			return nil, fmt.Errorf("archive expands past %d bytes", MaxRepoBytes)
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", zf.Name, err)
		}
		// LimitReader is belt-and-braces against a lying header.
		data, err := io.ReadAll(io.LimitReader(rc, MaxRepoFileBytes+1))
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", zf.Name, err)
		}
		files = append(files, RepoFile{Path: zf.Name, Data: data})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("archive contains no files")
	}
	return files, nil
}

// GitSource is the Option-A seam: fetch a repo by URL. It is intentionally not
// wired up — cloning needs network, and enabling it is a deliberate,
// clearly-labelled decision that must keep the fetch on the control plane and
// off the runner. Kept as a typed stub so the API/UI can already reference it.
type GitSource struct {
	URL string
}

// ErrGitSourceDisabled is returned until Option A is explicitly enabled.
var ErrGitSourceDisabled = fmt.Errorf(
	"git sources are not enabled: cloning requires network egress on the control plane; " +
		"upload a folder or zip instead (see docs/repos-plan.md)")

func (g GitSource) Fetch(ctx context.Context) ([]RepoFile, error) {
	return nil, ErrGitSourceDisabled
}

func (g GitSource) Describe() string { return "git: " + g.URL }

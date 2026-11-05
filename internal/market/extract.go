package market

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// secureTarballPath normalises an archive entry name and rejects absolute
// paths, drive letters, stream separators and any component that escapes the
// target; it mirrors internal/plugins/install.go secureZipPath.
func secureTarballPath(name string) (string, error) {
	n := strings.ReplaceAll(name, "\\", "/")
	if n == "" {
		return "", nil
	}
	if strings.ContainsRune(n, 0) {
		return "", fmt.Errorf("market: tarball entry %q contains a NUL byte", name)
	}
	if strings.HasPrefix(n, "/") || path.IsAbs(n) || filepath.VolumeName(name) != "" || strings.Contains(n, ":") {
		return "", fmt.Errorf("market: tarball entry %q must be a relative path", name)
	}
	clean := path.Clean(n)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("market: tarball entry %q escapes the staging directory", name)
	}
	return clean, nil
}

// extractTarball writes a tar.gz archive below dst, rejecting symlinks, links,
// special files, traversal escapes and oversized expansions.
func extractTarball(data []byte, dst string) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("market: open tarball: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	root := filepath.Clean(dst)
	var total uint64
	entries := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("market: tarball: %w", err)
		}
		if hdr == nil || hdr.Typeflag == tar.TypeXGlobalHeader || hdr.Typeflag == tar.TypeXHeader {
			continue
		}
		entries++
		if entries > maxTarEntries {
			return fmt.Errorf("market: tarball has more than %d entries", maxTarEntries)
		}
		rel, err := secureTarballPath(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(rel))
		if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return fmt.Errorf("market: tarball entry %q escapes the staging directory", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("market: tarball: create %q: %w", target, err)
			}
			continue
		case tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return fmt.Errorf("market: tarball entry %q is not a regular file", hdr.Name)
		case tar.TypeReg:
			// handled below
		default:
			return fmt.Errorf("market: tarball entry %q has unsupported type %d", hdr.Name, hdr.Typeflag)
		}
		if hdr.Size < 0 {
			return fmt.Errorf("market: tarball entry %q: negative size", hdr.Name)
		}
		size := uint64(hdr.Size)
		if size > MaxExtractedBytes || total > MaxExtractedBytes-size {
			return fmt.Errorf("market: tarball expands beyond %d bytes", MaxExtractedBytes)
		}
		total += size
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("market: tarball: create %q: %w", filepath.Dir(target), err)
		}
		perm := hdr.FileInfo().Mode().Perm()
		if perm == 0 {
			perm = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
		if err != nil {
			return fmt.Errorf("market: tarball: create %q: %w", target, err)
		}
		n, copyErr := io.Copy(out, io.LimitReader(tr, hdr.Size+1))
		closeErr := out.Close()
		if copyErr != nil {
			return fmt.Errorf("market: tarball: write %q: %w", target, copyErr)
		}
		if n > hdr.Size {
			return fmt.Errorf("market: tarball entry %q exceeds its declared size", hdr.Name)
		}
		if closeErr != nil {
			return fmt.Errorf("market: tarball: close %q: %w", target, closeErr)
		}
	}
	return nil
}

// copyTree mirrors internal/plugins/install.go copyTree: regular files and
// directories only, symlinks and special files rejected.
func copyTree(src, dst string) error {
	src = filepath.Clean(src)
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("market: copy %q: %w", p, err)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("market: copy: symlink %q is not allowed", p)
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return fmt.Errorf("market: copy %q: %w", p, err)
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("market: copy: unsupported file %q", p)
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("market: copy: open %q: %w", src, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("market: copy: stat %q: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("market: copy: create %q: %w", filepath.Dir(dst), err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("market: copy: create %q: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("market: copy: write %q: %w", dst, err)
	}
	return out.Close()
}

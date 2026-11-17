package nest

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Java comes from Eclipse Temurin (Adoptium), one folder per major version in java/<major>,
// downloaded the first time a server needs it and checked against Adoptium's SHA-256.

func javaArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "aarch64"
	case "arm":
		return "arm"
	case "386":
		return "x32"
	}
	return runtime.GOARCH
}

// javaPath is the java program for a major version, or "" until it's been downloaded.
func javaPath(major int) string {
	if j := os.Getenv("MINENEST_JAVA"); j != "" { // tests: a stand-in for java, nothing downloaded
		return j
	}
	dir := homePath("java", fmt.Sprint(major))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name(), "bin", exeName("java"))
		if exists(p) {
			return p
		}
	}
	return ""
}

type adoptiumAsset struct {
	Binary struct {
		Package struct {
			Link     string `json:"link"`
			Checksum string `json:"checksum"`
			Size     int64  `json:"size"`
			Name     string `json:"name"`
		} `json:"package"`
	} `json:"binary"`
	ReleaseName string `json:"release_name"`
}

// ensureJava returns the path of java <major>, downloading it first if it isn't here.
func ensureJava(ctx context.Context, major int, job *Job) (string, error) {
	if major <= 0 {
		major = 21
	}
	if p := javaPath(major); p != "" {
		return p, nil
	}
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "mac"
	}
	var assets []adoptiumAsset
	var err error
	for _, image := range []string{"jre", "jdk"} {
		url := fmt.Sprintf("https://api.adoptium.net/v3/assets/latest/%d/hotspot?architecture=%s&image_type=%s&os=%s&vendor=eclipse",
			major, javaArch(), image, osName)
		assets = nil
		if err = getJSON(ctx, url, 0, &assets); err == nil && len(assets) > 0 {
			break
		}
	}
	if len(assets) == 0 {
		if err == nil {
			err = fmt.Errorf("Eclipse Temurin has no Java %d for this computer (%s %s)", major, runtime.GOOS, javaArch())
		}
		return "", err
	}
	pkg := assets[0].Binary.Package
	dir := homePath("java", fmt.Sprint(major))
	archive := homePath("cache", pkg.Name)
	job.Say("Downloading Java %d", major)
	if err := download(ctx, pkg.Link, archive, Hash{"sha256", pkg.Checksum}, job, fmt.Sprintf("Java %d", major)); err != nil {
		return "", err
	}
	defer os.Remove(archive)
	job.Say("Unpacking Java %d", major)
	tmp := dir + ".unpacking"
	os.RemoveAll(tmp)
	if strings.HasSuffix(pkg.Name, ".zip") {
		err = unzip(archive, tmp, 0)
	} else {
		err = untgz(archive, tmp)
	}
	if err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("unpacking Java %d: %v", major, err)
	}
	os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	if p := javaPath(major); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("Java %d was unpacked but its java program isn't where it should be", major)
}

// unzip unpacks src into dest, refusing any entry that would land outside it. strip drops that
// many leading path parts from every entry.
func unzip(src, dest string, strip int) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		name := stripParts(f.Name, strip)
		if name == "" {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		if !within(dest, target) {
			return fmt.Errorf("the archive has a file outside its folder (%s)", f.Name)
		}
		// Windows' own zip tool writes folders as "name\", which IsDir doesn't see.
		if f.FileInfo().IsDir() || strings.HasSuffix(name, "/") {
			os.MkdirAll(target, 0o755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		mode := f.Mode().Perm() | 0o600
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func stripParts(name string, n int) string {
	name = strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "/")
	for i := 0; i < n; i++ {
		j := strings.Index(name, "/")
		if j < 0 {
			return ""
		}
		name = name[j+1:]
	}
	return name
}

func untgz(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.FromSlash(h.Name))
		if !within(dest, target) {
			return fmt.Errorf("the archive has a file outside its folder (%s)", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0o755)
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode).Perm()|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			out.Close()
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Only links that stay inside the folder (Temurin has a few, like legal/ ones).
			link := filepath.Join(filepath.Dir(target), h.Linkname)
			if within(dest, link) {
				os.MkdirAll(filepath.Dir(target), 0o755)
				os.Symlink(h.Linkname, target)
			}
		}
	}
}

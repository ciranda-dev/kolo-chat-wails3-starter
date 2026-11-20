package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestVersionIsDevWithoutLdflags(t *testing.T) {
	if version != "dev" {
		t.Fatalf("a build without -ldflags says %q, want dev", version)
	}
}

// versionFiles are the packaging files that carry the version, each with the pattern its version sits in.
var versionFiles = map[string]*regexp.Regexp{
	"build/config.yml":                   regexp.MustCompile(`(?m)^  version: "([^"]*)"`),
	"build/darwin/Info.plist":            regexp.MustCompile(`<string>(\d+\.\d+\.\d+)</string>\s*<key>CFBundleVersion`),
	"build/darwin/Info.dev.plist":        regexp.MustCompile(`<string>(\d+\.\d+\.\d+)</string>\s*<key>CFBundleVersion`),
	"build/linux/nfpm/nfpm.yaml":         regexp.MustCompile(`(?m)^version: "([^"]*)"`),
	"build/windows/info.json":            regexp.MustCompile(`"file_version": "([^"]*)"`),
	"build/windows/msix/template.xml":    regexp.MustCompile(`\bVersion="([^"]*)"\s+PackageDescription`),
	"build/windows/nsis/wails_tools.nsh": regexp.MustCompile(`INFO_PRODUCTVERSION "([^"]*)"`),
}

func TestPackagingFilesHoldNoVersion(t *testing.T) {
	for file, re := range versionFiles {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		m := re.FindSubmatch(b)
		if m == nil {
			t.Errorf("%s: no version found", file)
		} else if v := string(m[1]); v != "0.0.0" && v != "0.0.0.0" {
			t.Errorf("%s holds the version %s, want the placeholder 0.0.0", file, v)
		}
	}
}

func TestBuildsSetTheVersionFromTheTag(t *testing.T) {
	for _, file := range []string{"build/darwin/Taskfile.yml", "build/linux/Taskfile.yml", "build/windows/Taskfile.yml"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "-X main.version={{.VERSION}}") {
			t.Errorf("%s does not build the version in", file)
		}
	}
}

func TestStampVersionWritesTheTag(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	files := []string{"build/windows/kolo-chat.exe.manifest", "build/windows/wails.exe.manifest", "build/windows/msix/app_manifest.xml"}
	for file := range versionFiles {
		files = append(files, file)
	}
	for _, file := range files {
		copyTestFile(t, file, filepath.Join(dir, file))
	}
	if out, err := exec.Command("bash", "scripts/stamp-version.sh", "v1.2.3-4-gabc1234", dir).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := map[string]string{
		"build/config.yml":                   "1.2.3-4-gabc1234",
		"build/darwin/Info.plist":            "1.2.3",
		"build/darwin/Info.dev.plist":        "1.2.3",
		"build/linux/nfpm/nfpm.yaml":         "1.2.3-4-gabc1234",
		"build/windows/info.json":            "1.2.3",
		"build/windows/msix/template.xml":    "1.2.3.0",
		"build/windows/nsis/wails_tools.nsh": "1.2.3",
	}
	for file, v := range want {
		b, _ := os.ReadFile(filepath.Join(dir, file))
		if m := versionFiles[file].FindSubmatch(b); m == nil || string(m[1]) != v {
			t.Errorf("%s: version %q, want %q", file, m, v)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "build/windows/msix/app_manifest.xml"))
	if !strings.Contains(string(b), `Version="1.2.3.0"`) {
		t.Error("app_manifest.xml was not stamped")
	}
	for _, f := range []string{"kolo-chat.exe.manifest", "wails.exe.manifest"} {
		b, _ = os.ReadFile(filepath.Join(dir, "build/windows", f))
		if !strings.Contains(string(b), `name="dev.ciranda.kolochat" version="1.2.3"`) || !strings.Contains(string(b), `version="6.0.0.0"`) {
			t.Errorf("%s was stamped wrongly:\n%s", f, b)
		}
	}
	b, _ = os.ReadFile(filepath.Join(dir, "build/windows/info.json"))
	if !strings.Contains(string(b), `"ProductVersion": "1.2.3-4-gabc1234"`) {
		t.Error("info.json ProductVersion was not stamped")
	}
}

func copyTestFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

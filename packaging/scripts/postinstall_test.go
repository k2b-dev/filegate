package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostinstallAddsFgAliasToBashrcWhenRequested(t *testing.T) {
	home := t.TempDir()
	out, err := runPostinstall(t, home, "/bin/bash", true)
	if err != nil {
		t.Fatalf("postinstall failed: %v\n%s", err, out)
	}

	bashrc := filepath.Join(home, ".bashrc")
	body := readFile(t, bashrc)
	if !strings.Contains(body, "alias fg='filegate'") {
		t.Fatalf("%s=%q, want fg alias", bashrc, body)
	}
	if !strings.Contains(out, "Added fg alias") {
		t.Fatalf("output=%q, want alias confirmation", out)
	}
}

func TestPostinstallAddsFgAliasToZshrcWhenRequested(t *testing.T) {
	home := t.TempDir()
	out, err := runPostinstall(t, home, "/bin/zsh", true)
	if err != nil {
		t.Fatalf("postinstall failed: %v\n%s", err, out)
	}

	zshrc := filepath.Join(home, ".zshrc")
	body := readFile(t, zshrc)
	if !strings.Contains(body, "alias fg='filegate'") {
		t.Fatalf("%s=%q, want fg alias", zshrc, body)
	}
}

func TestPostinstallInstallsFgCommandLink(t *testing.T) {
	home := t.TempDir()
	out, binDir, err := runPostinstallWithPaths(t, home, "/bin/bash", false)
	if err != nil {
		t.Fatalf("postinstall failed: %v\n%s", err, out)
	}

	link := filepath.Join(binDir, "fg")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink %s: %v", link, err)
	}
	if target != "filegate" {
		t.Fatalf("fg link target=%q, want filegate", target)
	}
}

func TestPostinstallPrintsAliasSnippetForOtherShell(t *testing.T) {
	home := t.TempDir()
	out, err := runPostinstall(t, home, "/usr/bin/fish", true)
	if err != nil {
		t.Fatalf("postinstall failed: %v\n%s", err, out)
	}

	if !strings.Contains(out, "alias fg='filegate'") {
		t.Fatalf("output=%q, want alias snippet", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".bashrc")); !os.IsNotExist(err) {
		t.Fatalf(".bashrc stat error=%v, want no bashrc", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".zshrc")); !os.IsNotExist(err) {
		t.Fatalf(".zshrc stat error=%v, want no zshrc", err)
	}
}

func TestPostinstallLeavesShellConfigUntouchedWithoutAliasOption(t *testing.T) {
	home := t.TempDir()
	out, err := runPostinstall(t, home, "/bin/bash", false)
	if err != nil {
		t.Fatalf("postinstall failed: %v\n%s", err, out)
	}

	if _, err := os.Stat(filepath.Join(home, ".bashrc")); !os.IsNotExist(err) {
		t.Fatalf(".bashrc stat error=%v, want no bashrc", err)
	}
}

func runPostinstall(t *testing.T, home, shell string, installAlias bool) (string, error) {
	t.Helper()
	out, _, err := runPostinstallWithPaths(t, home, shell, installAlias)
	return out, err
}

func runPostinstallWithPaths(t *testing.T, home, shell string, installAlias bool) (string, string, error) {
	t.Helper()

	root := t.TempDir()
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "id"), "#!/bin/sh\nif [ \"$1\" = \"-u\" ] && [ \"$2\" = \"filegate\" ]; then exit 1; fi\nif [ \"$1\" = \"-u\" ]; then echo 1000; exit 0; fi\nexit 1\n")
	writeExecutable(t, filepath.Join(binDir, "systemctl"), "#!/bin/sh\nexit 0\n")
	installBinDir := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(installBinDir, 0o755); err != nil {
		t.Fatalf("mkdir install bindir: %v", err)
	}
	writeExecutable(t, filepath.Join(installBinDir, "filegate"), "#!/bin/sh\nexit 0\n")

	cmd := exec.Command("sh", "./postinstall.sh")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+home,
		"SHELL="+shell,
		"FILEGATE_ETC_DIR="+filepath.Join(root, "etc", "filegate"),
		"FILEGATE_STATE_DIR="+filepath.Join(root, "var", "lib", "filegate"),
		"FILEGATE_DATA_DIR="+filepath.Join(root, "data"),
		"FILEGATE_LOG_DIR="+filepath.Join(root, "var", "log", "filegate"),
		"FILEGATE_BINDIR="+installBinDir,
	)
	if installAlias {
		cmd.Env = append(cmd.Env, "FILEGATE_INSTALL_ALIAS_FG=1")
	}
	out, err := cmd.CombinedOutput()
	return string(out), installBinDir, err
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

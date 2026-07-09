package exporter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"os/exec"
	"strings"
)

// resolveConnectURI turns the kvm:// location into a libvirt connection URI and
// tells whether it is remote. It mirrors the importer's resolver (kept separate
// so the two connectors stay independent binaries).
//
//	kvm:///system              -> qemu:///system              (local)
//	kvm://[user@]host/system   -> qemu+ssh://[user@]host/system (remote)
func resolveConnectURI(loc string) (string, error) {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return "", fmt.Errorf("missing required 'location'")
	}
	parsed, err := url.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("invalid location %q: %w", loc, err)
	}
	if parsed.Scheme != "kvm" {
		return "", fmt.Errorf("location must use the kvm:// scheme, got %q", loc)
	}
	transport := strings.Trim(parsed.Path, "/")
	if transport != "system" && transport != "session" {
		return "", fmt.Errorf("location path must be /system or /session, got %q", parsed.Path)
	}
	if parsed.Host == "" {
		return fmt.Sprintf("qemu:///%s", transport), nil
	}
	userinfo := ""
	if parsed.User != nil && parsed.User.Username() != "" {
		userinfo = parsed.User.Username() + "@"
	}
	return fmt.Sprintf("qemu+ssh://%s%s/%s", userinfo, parsed.Host, transport), nil
}

// writeAccess abstracts writing the restored files, locally or over SSH.
type writeAccess interface {
	ping(ctx context.Context) error
	mkdirAll(ctx context.Context, dir string) error
	writeFile(ctx context.Context, path string, r io.Reader) error
}

func newWriteAccess(connectURI string) (writeAccess, error) {
	u, err := url.Parse(connectURI)
	if err != nil {
		return nil, fmt.Errorf("parsing connect URI %q: %w", connectURI, err)
	}
	remote := strings.Contains(u.Scheme, "ssh") || (u.Hostname() != "" && !isLocalHost(u.Hostname()))
	if !remote {
		return localWrite{}, nil
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("remote connect URI %q has no host", connectURI)
	}
	target := host
	if user := u.User.Username(); user != "" {
		target = user + "@" + host
	}
	return sshWrite{target: target, port: u.Port()}, nil
}

func isLocalHost(host string) bool {
	switch host {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// --- local write ------------------------------------------------------------

type localWrite struct{}

func (localWrite) ping(ctx context.Context) error { return nil }

func (localWrite) mkdirAll(ctx context.Context, dir string) error {
	return os.MkdirAll(dir, 0o755)
}

func (localWrite) writeFile(ctx context.Context, path string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// --- SSH write --------------------------------------------------------------

type sshWrite struct {
	target string // [user@]host
	port   string
}

func (s sshWrite) sshArgs(remoteCmd string) []string {
	// -C compresses the stream (helps raw disk images full of zeros).
	args := []string{"-o", "BatchMode=yes", "-C"}
	if s.port != "" {
		args = append(args, "-p", s.port)
	}
	return append(args, s.target, remoteCmd)
}

func (s sshWrite) run(ctx context.Context, remoteCmd string, stdin io.Reader) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", s.sshArgs(remoteCmd)...)
	cmd.Stdin = stdin
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh %s: %w: %s", remoteCmd, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (s sshWrite) ping(ctx context.Context) error {
	return s.run(ctx, "true", nil)
}

func (s sshWrite) mkdirAll(ctx context.Context, dir string) error {
	return s.run(ctx, fmt.Sprintf("mkdir -p -- %s", shellQuote(dir)), nil)
}

func (s sshWrite) writeFile(ctx context.Context, path string, r io.Reader) error {
	// Create the parent dir and stream the content into the file in one shot.
	remote := fmt.Sprintf("mkdir -p -- %s && cat > %s", shellQuote(pathpkg.Dir(path)), shellQuote(path))
	return s.run(ctx, remote, r)
}

// shellQuote single-quotes a string for safe interpolation into a remote shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

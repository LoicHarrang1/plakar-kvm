package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PlakarKorp/kloset/objects"
)

// storageAccess abstracts how disk images (and snapshot overlays) are inspected,
// read and removed. Two implementations:
//
//   - localAccess: the plugin runs on the KVM/DRBD node; images are on the local
//     filesystem or are local block devices.
//   - sshAccess: the plugin runs elsewhere (e.g. the Plakar server) and reaches
//     the hypervisor over SSH. The libvirt control plane already works remotely
//     through the qemu+ssh:// URI; this covers the data plane (reading the actual
//     disk bytes and removing overlay files) by shelling out to the ssh client.
type storageAccess interface {
	stat(ctx context.Context, src string) (objects.FileInfo, error)
	open(ctx context.Context, src string) (io.ReadCloser, error)
	removeTemp(ctx context.Context, path string) error
}

// newAccess decides between local and SSH access from the resolved libvirt
// connection URI: a qemu+ssh:// URI (or any non-local host) selects SSH. The SSH
// user/host/port are taken from the URI (which is derived from the kvm://
// location).
func newAccess(connectURI string) (storageAccess, error) {
	u, err := url.Parse(connectURI)
	if err != nil {
		return nil, fmt.Errorf("parsing connect URI %q: %w", connectURI, err)
	}

	remote := strings.Contains(u.Scheme, "ssh") || (u.Hostname() != "" && !isLocalHost(u.Hostname()))
	if !remote {
		return localAccess{}, nil
	}

	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("remote connect URI %q has no host", connectURI)
	}

	target := host
	if user := u.User.Username(); user != "" {
		target = user + "@" + host
	}
	return sshAccess{target: target, port: u.Port()}, nil
}

func isLocalHost(host string) bool {
	switch host {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// --- local access -----------------------------------------------------------

type localAccess struct{}

func (localAccess) stat(ctx context.Context, src string) (objects.FileInfo, error) {
	info, err := os.Stat(src)
	if err != nil {
		return objects.FileInfo{}, err
	}
	fi := objects.FileInfoFromStat(info)
	// Block devices report a zero st_size; query the real size.
	if fi.Size() == 0 && info.Mode()&os.ModeDevice != 0 {
		if sz, err := localBlockSize(ctx, src); err == nil {
			fi.Lsize = sz
		}
	}
	return fi, nil
}

func (localAccess) open(ctx context.Context, src string) (io.ReadCloser, error) {
	return os.Open(src)
}

func (localAccess) removeTemp(ctx context.Context, path string) error {
	return os.Remove(path)
}

func localBlockSize(ctx context.Context, dev string) (int64, error) {
	out, err := exec.CommandContext(ctx, "blockdev", "--getsize64", dev).Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// --- SSH access -------------------------------------------------------------

type sshAccess struct {
	target string // [user@]host
	port   string // optional
}

func (s sshAccess) sshArgs(remoteCmd string) []string {
	// -C compresses the stream: a big win for raw disks full of zeros.
	args := []string{"-o", "BatchMode=yes", "-C"}
	if s.port != "" {
		args = append(args, "-p", s.port)
	}
	return append(args, s.target, remoteCmd)
}

// runOut runs a remote command and returns its stdout.
func (s sshAccess) runOut(ctx context.Context, remoteCmd string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", s.sshArgs(remoteCmd)...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("ssh %s: %w: %s", remoteCmd, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (s sshAccess) stat(ctx context.Context, src string) (objects.FileInfo, error) {
	out, err := s.runOut(ctx, fmt.Sprintf("stat -c '%%s %%Y' -- %s", shellQuote(src)))
	if err != nil {
		return objects.FileInfo{}, err
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 {
		return objects.FileInfo{}, fmt.Errorf("unexpected stat output %q for %s", string(out), src)
	}
	size, _ := strconv.ParseInt(fields[0], 10, 64)
	mtime, _ := strconv.ParseInt(fields[1], 10, 64)
	// Block devices report size 0; query the real size.
	if size == 0 {
		if o, err := s.runOut(ctx, fmt.Sprintf("blockdev --getsize64 %s", shellQuote(src))); err == nil {
			size, _ = strconv.ParseInt(strings.TrimSpace(string(o)), 10, 64)
		}
	}
	return objects.NewFileInfo(pathpkg.Base(src), size, 0o644, time.Unix(mtime, 0), 0, 0, 0, 0, 1), nil
}

func (s sshAccess) open(ctx context.Context, src string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "ssh", s.sshArgs(fmt.Sprintf("cat -- %s", shellQuote(src)))...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdReadCloser{cmd: cmd, out: stdout, stderr: &stderr}, nil
}

func (s sshAccess) removeTemp(ctx context.Context, path string) error {
	_, err := s.runOut(ctx, fmt.Sprintf("rm -f -- %s", shellQuote(path)))
	return err
}

// cmdReadCloser adapts a running command's stdout into an io.ReadCloser. It
// turns a non-zero exit into a Read error (so a failed/truncated transfer is
// surfaced instead of looking like a clean EOF), and kills the command if the
// consumer stops reading early.
type cmdReadCloser struct {
	cmd    *exec.Cmd
	out    io.ReadCloser
	stderr *bytes.Buffer

	waitOnce sync.Once
	waitErr  error
	eof      bool
}

func (c *cmdReadCloser) wait() error {
	c.waitOnce.Do(func() {
		if err := c.cmd.Wait(); err != nil {
			c.waitErr = fmt.Errorf("%w: %s", err, strings.TrimSpace(c.stderr.String()))
		}
	})
	return c.waitErr
}

func (c *cmdReadCloser) Read(p []byte) (int, error) {
	n, err := c.out.Read(p)
	if errors.Is(err, io.EOF) {
		c.eof = true
		if werr := c.wait(); werr != nil {
			return n, werr
		}
	}
	return n, err
}

func (c *cmdReadCloser) Close() error {
	if !c.eof && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	werr := c.wait()
	if c.eof {
		return werr // completed: report any transfer error
	}
	return nil // aborted early by the consumer: not an error
}

// shellQuote single-quotes a string for safe interpolation into a remote shell
// command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

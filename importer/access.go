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

// storageAccess abstracts how disk images are inspected, converted and read.
// It has two implementations:
//
//   - localAccess: the plugin runs on the KVM/DRBD node; images are on the
//     local filesystem or are local block devices.
//   - sshAccess: the plugin runs elsewhere (e.g. the Plakar server) and reaches
//     the hypervisor over SSH. The libvirt control plane already works remotely
//     through the qemu+ssh:// URI; this covers the data plane (reading the
//     actual disk bytes) by shelling out to the ssh client.
//
// convertToTemp/statTemp/openTemp/removeTemp operate on a temporary consistent
// copy (used by the fsfreeze strategy); open/stat operate on the live source
// (used by the crash strategy).
type storageAccess interface {
	stat(ctx context.Context, src string) (objects.FileInfo, error)
	open(ctx context.Context, src string) (io.ReadCloser, error)
	convertToTemp(ctx context.Context, src string) (tmp string, err error)
	statTemp(ctx context.Context, tmp string) (objects.FileInfo, error)
	openTemp(ctx context.Context, tmp string) (io.ReadCloser, error)
	removeTemp(ctx context.Context, tmp string) error
}

// newAccess decides between local and SSH access based on the resolved libvirt
// connection URI. A qemu+ssh:// URI (or any non-local host) selects SSH access;
// ssh_user / ssh_port config keys override what the URI carries.
func newAccess(connectURI string, config map[string]string) (storageAccess, bool, error) {
	u, err := url.Parse(connectURI)
	if err != nil {
		return nil, false, fmt.Errorf("parsing connect URI %q: %w", connectURI, err)
	}

	// Temp dir for consistent copies (fsfreeze mode). Default to /var/tmp, which
	// is disk-backed on all distros — /tmp is often tmpfs (RAM) and would blow up
	// on large disk images.
	tmpDir := strings.TrimSpace(config["tmp_dir"])
	if tmpDir == "" {
		tmpDir = "/var/tmp"
	}

	remote := strings.Contains(u.Scheme, "ssh") || (u.Hostname() != "" && !isLocalHost(u.Hostname()))
	if !remote {
		return localAccess{tmpDir: tmpDir}, false, nil
	}

	host := u.Hostname()
	if host == "" {
		return nil, false, fmt.Errorf("remote connect URI %q has no host", connectURI)
	}

	user := u.User.Username()
	if v := strings.TrimSpace(config["ssh_user"]); v != "" {
		user = v
	}
	port := u.Port()
	if v := strings.TrimSpace(config["ssh_port"]); v != "" {
		port = v
	}

	target := host
	if user != "" {
		target = user + "@" + host
	}
	return sshAccess{target: target, port: port, tmpDir: tmpDir}, true, nil
}

func isLocalHost(host string) bool {
	switch host {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// --- local access -----------------------------------------------------------

type localAccess struct{ tmpDir string }

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

func (la localAccess) convertToTemp(ctx context.Context, src string) (string, error) {
	tmp, err := os.CreateTemp(la.tmpDir, "plakar-kvm-*.qcow2")
	if err != nil {
		return "", err
	}
	tmp.Close()
	cmd := exec.CommandContext(ctx, "qemu-img", "convert", "-O", "qcow2", src, tmp.Name())
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("qemu-img convert: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return tmp.Name(), nil
}

func (localAccess) statTemp(ctx context.Context, tmp string) (objects.FileInfo, error) {
	info, err := os.Stat(tmp)
	if err != nil {
		return objects.FileInfo{}, err
	}
	return objects.FileInfoFromStat(info), nil
}

func (localAccess) openTemp(ctx context.Context, tmp string) (io.ReadCloser, error) {
	return newDeletingFileReader(tmp)
}

func (localAccess) removeTemp(ctx context.Context, tmp string) error {
	return os.Remove(tmp)
}

func localBlockSize(ctx context.Context, dev string) (int64, error) {
	out, err := exec.CommandContext(ctx, "blockdev", "--getsize64", dev).Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// deletingFileReader reads a local temporary file and removes it on Close.
type deletingFileReader struct{ f *os.File }

func newDeletingFileReader(name string) (io.ReadCloser, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return &deletingFileReader{f: f}, nil
}

func (r *deletingFileReader) Read(p []byte) (int, error) { return r.f.Read(p) }

func (r *deletingFileReader) Close() error {
	name := r.f.Name()
	err := r.f.Close()
	os.Remove(name)
	return err
}

// --- SSH access -------------------------------------------------------------

type sshAccess struct {
	target string // [user@]host
	port   string // optional
	tmpDir string // remote dir for consistent copies (fsfreeze mode)
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

// stream starts a remote command and exposes its stdout as a ReadCloser. onClose
// (if set) runs after the command finishes, e.g. to delete a remote temp file.
func (s sshAccess) stream(ctx context.Context, remoteCmd string, onClose func()) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "ssh", s.sshArgs(remoteCmd)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdReadCloser{cmd: cmd, out: stdout, stderr: &stderr, onClose: onClose}, nil
}

func (s sshAccess) stat(ctx context.Context, src string) (objects.FileInfo, error) {
	size, mtime, err := s.statPath(ctx, src)
	if err != nil {
		return objects.FileInfo{}, err
	}
	return objects.NewFileInfo(pathpkg.Base(src), size, 0o644, mtime, 0, 0, 0, 0, 1), nil
}

func (s sshAccess) statTemp(ctx context.Context, tmp string) (objects.FileInfo, error) {
	return s.stat(ctx, tmp)
}

func (s sshAccess) statPath(ctx context.Context, src string) (int64, time.Time, error) {
	out, err := s.runOut(ctx, fmt.Sprintf("stat -c '%%s %%Y' -- %s", shellQuote(src)))
	if err != nil {
		return 0, time.Time{}, err
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 {
		return 0, time.Time{}, fmt.Errorf("unexpected stat output %q for %s", string(out), src)
	}
	size, _ := strconv.ParseInt(fields[0], 10, 64)
	mtime, _ := strconv.ParseInt(fields[1], 10, 64)
	// Block devices report size 0; query the real size.
	if size == 0 {
		if o, err := s.runOut(ctx, fmt.Sprintf("blockdev --getsize64 %s", shellQuote(src))); err == nil {
			size, _ = strconv.ParseInt(strings.TrimSpace(string(o)), 10, 64)
		}
	}
	return size, time.Unix(mtime, 0), nil
}

func (s sshAccess) open(ctx context.Context, src string) (io.ReadCloser, error) {
	return s.stream(ctx, fmt.Sprintf("cat -- %s", shellQuote(src)), nil)
}

func (s sshAccess) convertToTemp(ctx context.Context, src string) (string, error) {
	// mktemp + qemu-img convert on the remote host; echo the temp path back.
	// -p <tmpDir> keeps the (potentially large) copy off a tmpfs /tmp.
	remote := fmt.Sprintf(
		`tmp=$(mktemp -p %s --suffix=.qcow2) && qemu-img convert -O qcow2 -- %s "$tmp" && printf %%s "$tmp"`,
		shellQuote(s.tmpDir), shellQuote(src),
	)
	out, err := s.runOut(ctx, remote)
	if err != nil {
		return "", fmt.Errorf("remote qemu-img convert of %s: %w", src, err)
	}
	tmp := strings.TrimSpace(string(out))
	if tmp == "" {
		return "", fmt.Errorf("remote qemu-img convert of %s returned no temp path", src)
	}
	return tmp, nil
}

func (s sshAccess) openTemp(ctx context.Context, tmp string) (io.ReadCloser, error) {
	return s.stream(ctx, fmt.Sprintf("cat -- %s", shellQuote(tmp)), func() {
		// Best-effort cleanup with a fresh context (the read ctx may be done).
		_, _ = s.runOut(context.Background(), fmt.Sprintf("rm -f -- %s", shellQuote(tmp)))
	})
}

func (s sshAccess) removeTemp(ctx context.Context, tmp string) error {
	_, err := s.runOut(ctx, fmt.Sprintf("rm -f -- %s", shellQuote(tmp)))
	return err
}

// cmdReadCloser adapts a running command's stdout into an io.ReadCloser. It
// turns a non-zero exit into a Read error (so a failed/truncated transfer is
// surfaced instead of looking like a clean EOF), and kills the command if the
// consumer stops reading early.
type cmdReadCloser struct {
	cmd     *exec.Cmd
	out     io.ReadCloser
	stderr  *bytes.Buffer
	onClose func()

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
	if c.onClose != nil {
		c.onClose()
	}
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

//go:build linux

package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/k2b-dev/filegate/v6/domain"
	"golang.org/x/sys/unix"
)

const workerMarker = "--filegate-execution-worker"
const packetLimit = 64 << 10

var workers = make(chan struct{}, 32)

type executionRequest struct {
	Op         string          `json:"op"`
	Path       string          `json:"path,omitempty"`
	Target     string          `json:"target,omitempty"`
	Flags      int             `json:"flags,omitempty"`
	Mode       os.FileMode     `json:"mode,omitempty"`
	Replace    bool            `json:"replace,omitempty"`
	Recursive  bool            `json:"recursive,omitempty"`
	UID        int             `json:"uid,omitempty"`
	GID        int             `json:"gid,omitempty"`
	Groups     []uint32        `json:"groups,omitempty"`
	Scope      domain.ACLScope `json:"scope,omitempty"`
	ACL        domain.ACL      `json:"acl,omitempty"`
	PrivateDev uint64          `json:"privateDev,omitempty"`
	PrivateIno uint64          `json:"privateIno,omitempty"`
}
type executionReply struct {
	Error string     `json:"error,omitempty"`
	Errno int        `json:"errno,omitempty"`
	Kind  string     `json:"kind,omitempty"`
	ACL   domain.ACL `json:"acl,omitempty"`
}

// packetReader belongs to one serialized connection. Reusing these bounded
// buffers keeps per-file transfers from allocating the maximum packet size.
type packetReader struct {
	payload   [packetLimit]byte
	ancillary [128]byte
}

type executionFiles struct {
	*Files
	conn     *net.UnixConn
	reader   packetReader
	cmd      *exec.Cmd
	mu       sync.Mutex
	once     sync.Once
	done     chan struct{}
	uid, gid uint32
}

func packetConn(file *os.File) (*net.UnixConn, error) {
	c, err := net.FileConn(file)
	if err != nil {
		return nil, err
	}
	u, ok := c.(*net.UnixConn)
	if !ok {
		c.Close()
		return nil, os.ErrInvalid
	}
	return u, nil
}
func sendPacket(c *net.UnixConn, value any, file *os.File) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) > packetLimit {
		return domain.ErrLimit
	}
	var rights []byte
	if file != nil {
		rights = unix.UnixRights(int(file.Fd()))
	}
	n, _, err := c.WriteMsgUnix(b, rights, nil)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}
func (r *packetReader) read(c *net.UnixConn, value any) (*os.File, error) {
	b, control := r.payload[:], r.ancillary[:unix.CmsgSpace(4*4)]
	n, on, flags, _, err := c.ReadMsgUnix(b, control)
	if err != nil {
		return nil, err
	}
	var descriptors []int
	messages, parseErr := unix.ParseSocketControlMessage(control[:on])
	if parseErr == nil {
		for _, m := range messages {
			fds, e := unix.ParseUnixRights(&m)
			if e != nil {
				parseErr = e
				break
			}
			descriptors = append(descriptors, fds...)
		}
	}
	fail := func(e error) (*os.File, error) {
		for _, fd := range descriptors {
			unix.Close(fd)
		}
		return nil, e
	}
	if parseErr != nil {
		return fail(parseErr)
	}
	if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || len(descriptors) > 1 {
		return fail(domain.ErrLimit)
	}
	if n == 0 {
		return fail(io.EOF)
	}
	if err = json.Unmarshal(b[:n], value); err != nil {
		return fail(err)
	}
	if len(descriptors) == 0 {
		return nil, nil
	}
	unix.CloseOnExec(descriptors[0])
	return os.NewFile(uintptr(descriptors[0]), "execution-descriptor"), nil
}

// WithExecution starts one bounded, fixed-credential worker. It never changes the
// credentials of a Go server thread. Close the returned scope after its operation.
func (f *Files) WithExecution(ctx context.Context, uid, gid uint32, groups []uint32) (domain.Files, func(), error) {
	if os.Geteuid() != 0 || uid == 0 || uid == ^uint32(0) || gid == ^uint32(0) || len(groups) > 256 {
		return nil, nil, fmt.Errorf("unix execution requires a root daemon and a non-root UID: %w", os.ErrPermission)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	select {
	case workers <- struct{}{}:
	default:
		return nil, nil, fmt.Errorf("unix execution capacity: %w", domain.ErrExecutionCapacity)
	}
	release := true
	defer func() {
		if release {
			<-workers
		}
	}()
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	parent, child := os.NewFile(uintptr(sockets[0]), "execution-parent"), os.NewFile(uintptr(sockets[1]), "execution-child")
	defer parent.Close()
	defer child.Close()
	conn, err := packetConn(parent)
	if err != nil {
		return nil, nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	binary, err := os.Open(executable)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	defer binary.Close()
	// An inherited executable descriptor also works when the test executable's
	// temporary parent directory is inaccessible to the requested identity.
	cmd := exec.Command("/proc/self/fd/5", workerMarker)
	cmd.Args[0] = executable
	cmd.ExtraFiles = []*os.File{child, f.root, binary}
	cmd.Env = []string{"GOMAXPROCS=1", "GOMEMLIMIT=32MiB"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err = cmd.Start(); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("start Unix execution worker: %w", err)
	}
	e := &executionFiles{Files: f, conn: conn, cmd: cmd, done: make(chan struct{}), uid: uid, gid: gid}
	release = false
	go func() { _ = cmd.Wait(); <-workers; close(e.done) }()
	go func() {
		select {
		case <-ctx.Done():
			_ = e.Close()
		case <-e.done:
		}
	}()
	_, _, err = e.call(executionRequest{Op: "init", UID: int(uid), GID: int(gid), Groups: groups, PrivateDev: f.privateDev, PrivateIno: f.privateIno}, nil)
	if err != nil {
		e.Close()
		return nil, nil, err
	}
	return e, func() { _ = e.Close() }, nil
}
func (e *executionFiles) Close() error {
	e.once.Do(func() {
		e.conn.Close()
		_ = e.cmd.Process.Kill()
	})
	<-e.done
	return nil
}
func (e *executionFiles) call(q executionRequest, file *os.File) (executionReply, *os.File, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.conn.SetDeadline(time.Now().Add(30 * time.Second))
	var reply executionReply
	if err := sendPacket(e.conn, q, file); err != nil {
		_ = e.Close()
		return reply, nil, err
	}
	output, err := e.reader.read(e.conn, &reply)
	if err != nil {
		_ = e.Close()
		return reply, nil, err
	}
	if reply.Error != "" {
		if output != nil {
			output.Close()
		}
		var cause error
		switch reply.Kind {
		case "permission":
			cause = os.ErrPermission
		case "notExist":
			cause = os.ErrNotExist
		case "invalid":
			cause = domain.ErrInvalid
		case "acl":
			cause = domain.ErrInvalidACL
		case "unsupported":
			cause = domain.ErrACLUnsupported
		case "limit":
			cause = domain.ErrLimit
		}
		if reply.Errno != 0 && cause == nil {
			cause = syscall.Errno(reply.Errno)
		}
		if cause == nil {
			cause = os.ErrInvalid
		}
		return reply, nil, fmt.Errorf("unix execution: %s: %w", reply.Error, cause)
	}
	return reply, output, nil
}
func privatePath(p string) bool   { return p == ".filegate" || strings.HasPrefix(p, ".filegate/") }
func privateFile(f *os.File) bool { return privatePath(f.Name()) }
func (e *executionFiles) Open(p string, flags int, mode os.FileMode) (*os.File, error) {
	if privatePath(p) {
		return e.Files.Open(p, flags, mode)
	}
	_, f, err := e.call(executionRequest{Op: "open", Path: p, Flags: flags, Mode: mode}, nil)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, io.ErrUnexpectedEOF
	}
	// Keep the public path on the descriptor for metadata dispatch.
	// Transfer ownership without two os.File finalizers referring to the same fd.
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	f.Close()
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), p), nil
}
func (e *executionFiles) MetadataOpen(p string) (*os.File, error) {
	if privatePath(p) {
		return e.Files.Open(p, os.O_RDONLY, 0)
	}
	_, f, err := e.call(executionRequest{Op: "open", Path: p, Flags: unix.O_PATH}, nil)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, io.ErrUnexpectedEOF
	}
	defer f.Close()
	fd, err := unix.Open("/proc/self/fd/"+strconv.FormatUint(uint64(f.Fd()), 10), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), p), nil
}
func (e *executionFiles) Stat(p string) (os.FileInfo, error) {
	f, err := e.MetadataOpen(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}
func (e *executionFiles) Mkdir(p string, mode os.FileMode) error {
	if privatePath(p) {
		return e.Files.Mkdir(p, mode)
	}
	_, _, err := e.call(executionRequest{Op: "mkdir", Path: p, Mode: mode}, nil)
	if err != nil {
		return err
	}
	return e.Files.Sync(path.Dir(p))
}
func (e *executionFiles) Remove(p string, recursive bool) error {
	if privatePath(p) {
		return e.Files.Remove(p, recursive)
	}
	_, _, err := e.call(executionRequest{Op: "remove", Path: p, Recursive: recursive}, nil)
	if err != nil {
		return err
	}
	return e.Files.Sync(path.Dir(p))
}
func (e *executionFiles) Chmod(f *os.File, mode os.FileMode) error {
	if privateFile(f) {
		return f.Chmod(mode)
	}
	_, _, err := e.call(executionRequest{Op: "chmod", Mode: mode}, f)
	return err
}
func (e *executionFiles) Chown(f *os.File, uid, gid int) error {
	if privateFile(f) {
		return f.Chown(uid, gid)
	}
	_, _, err := e.call(executionRequest{Op: "chown", UID: uid, GID: gid}, f)
	return err
}
func (e *executionFiles) GetACL(f *os.File, scope domain.ACLScope) (domain.ACL, error) {
	if privateFile(f) {
		return e.Files.GetACL(f, scope)
	}
	reply, _, err := e.call(executionRequest{Op: "getACL", Scope: scope}, f)
	return reply.ACL, err
}
func (e *executionFiles) SetACL(f *os.File, scope domain.ACLScope, acl domain.ACL) error {
	if privateFile(f) {
		return e.Files.SetACL(f, scope, acl)
	}
	_, _, err := e.call(executionRequest{Op: "setACL", Scope: scope, ACL: acl}, f)
	return err
}
func (e *executionFiles) ClearDefaultACL(f *os.File) error {
	if privateFile(f) {
		return e.Files.ClearDefaultACL(f)
	}
	_, _, err := e.call(executionRequest{Op: "clearACL"}, f)
	return err
}
func (e *executionFiles) Rename(a, b string, replace bool) error {
	if privatePath(a) && privatePath(b) {
		return e.Files.Rename(a, b, replace)
	}
	if !privatePath(a) && !privatePath(b) {
		_, _, err := e.call(executionRequest{Op: "rename", Path: a, Target: b, Replace: replace}, nil)
		if err != nil {
			return err
		}
		if err = e.Files.Sync(path.Dir(b)); err != nil {
			return err
		}
		return e.Files.Sync(path.Dir(a))
	}
	bridge := ".filegate/staging/exec-" + uuid.NewString()
	if err := e.Files.Mkdir(bridge, 0700); err != nil {
		return err
	}
	directory, err := e.Files.Open(bridge, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err = directory.Chown(int(e.uid), int(e.gid)); err != nil {
		e.Files.Remove(bridge, true)
		return err
	}
	item := bridge + "/item"
	// Cleanup only after the worker reply (or after killing and reaping it on an
	// interrupted exchange), so it cannot mutate a removed or reused directory.
	defer func() { _ = e.Files.Remove(bridge, true) }()
	if privatePath(a) {
		if err = e.Files.Rename(a, item, false); err != nil {
			return err
		}
		_, _, err = e.call(executionRequest{Op: "publish", Target: b, Replace: replace}, directory)
		if err != nil {
			if _, check := e.Files.Stat(item); check == nil {
				_ = e.Files.Rename(item, a, false)
			}
			return err
		}
		if err = e.Files.Sync(path.Dir(b)); err != nil {
			return err
		}
	} else {
		_, _, err = e.call(executionRequest{Op: "quarantine", Path: a}, directory)
		if err != nil {
			return err
		}
		if err = e.Files.Sync(path.Dir(a)); err != nil {
			return err
		}
		if err = e.Files.Rename(item, b, replace); err != nil {
			return err
		}
	}
	return directory.Sync()
}

// RunExecutionWorker handles the private worker entry point before CLI or test
// initialization. It accepts only inherited descriptors from its parent.
func RunExecutionWorker() (bool, error) {
	if len(os.Args) != 2 || os.Args[1] != workerMarker {
		return false, nil
	}
	if os.Geteuid() != 0 {
		return true, os.ErrPermission
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return true, err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return true, err
	}
	_ = unix.Close(5)
	socket := os.NewFile(3, "execution-socket")
	defer socket.Close()
	conn, err := packetConn(socket)
	if err != nil {
		return true, err
	}
	defer conn.Close()
	root := &Files{root: os.NewFile(4, "."), searchOnly: true}
	defer root.Close()
	initialized := false
	var reader packetReader
	for {
		var q executionRequest
		input, err := reader.read(conn, &q)
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err != nil {
			return true, err
		}
		var reply executionReply
		var output *os.File
		if !initialized {
			if q.Op != "init" || input != nil {
				err = os.ErrInvalid
			} else {
				err = establishIdentity(q)
				if err == nil {
					root.privateDev = q.PrivateDev
					root.privateIno = q.PrivateIno
					root.protectPrivate = true
					initialized = true
				}
			}
		} else {
			reply, output, err = executeRequest(root, q, input)
		}
		if input != nil {
			input.Close()
		}
		if err != nil {
			reply.Error = err.Error()
			var errno syscall.Errno
			if errors.As(err, &errno) {
				reply.Errno = int(errno)
			}
			switch {
			case errors.Is(err, os.ErrPermission):
				reply.Kind = "permission"
			case errors.Is(err, os.ErrNotExist):
				reply.Kind = "notExist"
			case errors.Is(err, domain.ErrInvalidACL):
				reply.Kind = "acl"
			case errors.Is(err, domain.ErrACLUnsupported):
				reply.Kind = "unsupported"
			case errors.Is(err, domain.ErrLimit):
				reply.Kind = "limit"
			case errors.Is(err, domain.ErrInvalid):
				reply.Kind = "invalid"
			}
		}
		sendErr := sendPacket(conn, reply, output)
		if output != nil {
			output.Close()
		}
		if sendErr != nil {
			return true, sendErr
		}
	}
}
func executeRequest(f *Files, q executionRequest, input *os.File) (executionReply, *os.File, error) {
	var reply executionReply
	if q.Path != "" && privatePath(q.Path) || q.Target != "" && privatePath(q.Target) {
		return reply, nil, os.ErrPermission
	}
	metadata := q.Op == "chmod" || q.Op == "chown" || q.Op == "getACL" || q.Op == "setACL" || q.Op == "clearACL" || q.Op == "publish" || q.Op == "quarantine"
	if metadata != (input != nil) {
		return reply, nil, os.ErrInvalid
	}
	var err error
	switch q.Op {
	case "open":
		var out *os.File
		out, err = f.Open(q.Path, q.Flags, q.Mode)
		return reply, out, err
	case "mkdir":
		err = f.Mkdir(q.Path, q.Mode)
	case "remove":
		err = f.Remove(q.Path, q.Recursive)
	case "rename":
		err = f.Rename(q.Path, q.Target, q.Replace)
	case "chmod":
		err = input.Chmod(q.Mode)
	case "chown":
		err = input.Chown(q.UID, q.GID)
	case "getACL":
		reply.ACL, err = f.GetACL(input, q.Scope)
	case "setACL":
		err = f.SetACL(input, q.Scope, q.ACL)
	case "clearACL":
		err = f.ClearDefaultACL(input)
	case "publish", "quarantine":
		p := q.Target
		if q.Op == "quarantine" {
			p = q.Path
		}
		var parent *os.File
		var name string
		parent, name, err = f.parent(p)
		if err != nil {
			break
		}
		defer parent.Close()
		flags := uint(unix.RENAME_NOREPLACE)
		if q.Replace {
			flags = 0
		}
		if q.Op == "publish" {
			err = unix.Renameat2(int(input.Fd()), "item", int(parent.Fd()), name, flags)
		} else {
			err = unix.Renameat2(int(parent.Fd()), name, int(input.Fd()), "item", unix.RENAME_NOREPLACE)
		}
	default:
		err = os.ErrInvalid
	}
	return reply, nil, err
}

// The helper starts privileged so another actor process cannot trace its Go
// runtime startup. Go's syscall credential wrappers update every OS thread;
// credentials are never changed in the server process. The uid transition
// clears dumpability before any actor-private descriptors are delegated.
func establishIdentity(q executionRequest) error {
	if q.UID <= 0 || uint64(q.UID) >= uint64(^uint32(0)) || q.GID < 0 || uint64(q.GID) >= uint64(^uint32(0)) || len(q.Groups) > 256 {
		return os.ErrPermission
	}
	dumpable, err := os.ReadFile("/proc/sys/fs/suid_dumpable")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(dumpable)) == "1" {
		return fmt.Errorf("unix execution requires fs.suid_dumpable=0 or 2: %w", os.ErrPermission)
	}
	groups := make([]int, len(q.Groups))
	for i, gid := range q.Groups {
		if gid == ^uint32(0) {
			return os.ErrInvalid
		}
		groups[i] = int(gid)
	}
	parentPID := os.Getppid()
	if err := syscall.Setgroups(groups); err != nil {
		return err
	}
	if err := syscall.Setgid(q.GID); err != nil {
		return err
	}
	if err := syscall.Setuid(q.UID); err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return err
	}
	// Linux clears the parent-death signal on a credential transition.
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0); err != nil {
		return err
	}
	if os.Getppid() != parentPID {
		return io.EOF
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	caps := [2]unix.CapUserData{}
	return unix.Capset(&header, &caps[0])
}

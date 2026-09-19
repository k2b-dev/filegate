//go:build linux

package filesystem

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/k2b-dev/filegate/v6/domain"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if handled, err := RunExecutionWorker(); handled {
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 3 && os.Args[1] == "--filegate-proc-probe" {
		if os.Geteuid() != 12345 {
			os.Exit(2)
		}
		_, err := os.Readlink(os.Args[2])
		if errors.Is(err, os.ErrPermission) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "expected same-UID proc fd denial, got: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
func executionFixture(t testing.TB) (*Files, *executionFiles) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root to establish execution identity")
	}
	dir := t.TempDir()
	// Ancestors above the configured root are intentionally inaccessible: root fd
	// is delegated, while all traversal inside it is checked under the actor.
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	f, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err = f.Mkdir(".filegate", 0700); err != nil {
		t.Fatal(err)
	}
	if err = f.SecurePrivate(); err != nil {
		t.Fatal(err)
	}
	if err = f.Mkdir(".filegate/staging", 0700); err != nil {
		t.Fatal(err)
	}
	scope, closeScope, err := f.WithExecution(context.Background(), 12345, 12345, []uint32{12346})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeScope)
	return f, scope.(*executionFiles)
}
func TestExecutionLeafTraversalAndGroups(t *testing.T) {
	f, e := executionFixture(t)
	dir := f.root.Name()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Mkdir(filepath.Join(dir, "search"), 0711))
	must(os.WriteFile(filepath.Join(dir, "search", "yes"), []byte("allowed"), 0644))
	must(os.WriteFile(filepath.Join(dir, "search", "no"), []byte("secret"), 0600))
	opened, err := e.Open("search/yes", os.O_RDONLY, 0)
	must(err)
	opened.Close()
	if _, err = e.Open("search/no", os.O_RDONLY, 0); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("protected leaf: %v", err)
	}
	if _, err = e.Open("search", os.O_RDONLY, 0); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("listing search-only directory: %v", err)
	}
	metadata, err := e.MetadataOpen("search")
	must(err)
	metadata.Close()
	must(os.Chown(filepath.Join(dir, "search"), 0, 12346))
	must(os.Chmod(filepath.Join(dir, "search"), 0770))
	must(e.Mkdir("search/group", 0700))
	info, err := f.Stat("search/group")
	must(err)
	_, _, uid, gid, _ := f.Identity(info)
	if uid != 12345 || gid != 12345 {
		t.Fatalf("unexpected owner %d:%d", uid, gid)
	}
}
func TestExecutionPublicationAndMetadata(t *testing.T) {
	f, e := executionFixture(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.Mkdir("target", 0770))
	target, err := f.Open("target", os.O_RDONLY, 0)
	must(err)
	must(target.Chown(12345, 12345))
	target.Close()
	stage, err := f.Open(".filegate/staging/content", os.O_CREATE|os.O_WRONLY, 0600)
	must(err)
	_, err = stage.WriteString("data")
	must(err)
	must(stage.Chown(12345, 12345))
	stage.Close()
	must(e.Rename(".filegate/staging/content", "target/file", false))
	leaf, err := e.Open("target/file", os.O_RDONLY, 0)
	must(err)
	if err = e.Chown(leaf, 0, 0); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("actor chown to root: %v", err)
	}
	must(e.Chmod(leaf, 0640))
	leaf.Close()
	must(e.Rename("target/file", ".filegate/staging/deleted", false))
	if _, err = f.Stat("target/file"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantine original: %v", err)
	}
	must(f.Remove(".filegate/staging/deleted", false))
	// Destination permission is checked at the actual rename syscall.
	stage, err = f.Open(".filegate/staging/denied", os.O_CREATE|os.O_WRONLY, 0600)
	must(err)
	stage.Close()
	if err = e.Rename(".filegate/staging/denied", "no-write", false); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("denied publication: %v", err)
	}
	if _, err = f.Stat(".filegate/staging/denied"); err != nil {
		t.Fatalf("failed publication lost stage: %v", err)
	}
}
func TestExecutionACLAndPrivateAlias(t *testing.T) {
	f, e := executionFixture(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.Mkdir("acl", 0700))
	directory, err := f.Open("acl", os.O_RDONLY, 0)
	must(err)
	defer directory.Close()
	uid := uint32(12345)
	must(f.SetACL(directory, domain.AccessACL, domain.ACL{Entries: []domain.ACLEntry{{Tag: domain.ACLOwner, Permissions: "rwx"}, {Tag: domain.ACLUser, ID: &uid, Permissions: "r-x"}, {Tag: domain.ACLOwningGroup, Permissions: "---"}, {Tag: domain.ACLMask, Permissions: "r-x"}, {Tag: domain.ACLOther, Permissions: "---"}}}))
	actor, err := e.Open("acl", os.O_RDONLY, 0)
	must(err)
	defer actor.Close()
	if err = e.ClearDefaultACL(actor); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("actor ACL mutation: %v", err)
	}
	// Move the private directory externally: inode checks still reject its alias.
	must(os.Rename(filepath.Join(f.root.Name(), ".filegate"), filepath.Join(f.root.Name(), "alias")))
	if _, err = e.MetadataOpen("alias"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("private alias: %v", err)
	}
}

func TestExecutionAllThreadCredentialsAndCancellation(t *testing.T) {
	f, e := executionFixture(t)
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", e.cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("worker has no threads")
	}
	for _, entry := range entries {
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/status", e.cmd.Process.Pid, entry.Name()))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(status), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			switch fields[0] {
			case "Uid:", "Gid:":
				for _, id := range fields[1:] {
					if id != "12345" {
						t.Fatalf("thread %s %s", entry.Name(), line)
					}
				}
			case "Groups:":
				if strings.Join(fields[1:], ",") != "12346" {
					t.Fatalf("thread supplementary groups: %s", line)
				}
			case "CapEff:", "CapPrm:", "CapAmb:":
				if fields[1] != "0000000000000000" {
					t.Fatalf("thread retained capabilities: %s", line)
				}
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	scope, closeScope, err := f.WithExecution(ctx, 12345, 12345, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeScope()
	cancel()
	select {
	case <-scope.(*executionFiles).done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled worker not reaped")
	}
	if _, err = scope.Open(".", os.O_RDONLY, 0); err == nil {
		t.Fatal("closed worker accepted operation")
	}
}
func TestExecutionBoundedCapacity(t *testing.T) {
	f, _ := executionFixture(t)
	var closeScopes []func()
	defer func() {
		for _, closeScope := range closeScopes {
			closeScope()
		}
	}()
	for i := 0; i < cap(workers); i++ {
		_, closeScope, err := f.WithExecution(context.Background(), 12345, 12345, nil)
		if errors.Is(err, domain.ErrExecutionCapacity) {
			if i != cap(workers)-1 {
				t.Fatalf("capacity reached early at %d", i)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		closeScopes = append(closeScopes, closeScope)
	}
	t.Fatal("execution scopes exceeded capacity")
}

func TestExecutionDescriptorLifetimeAndPoisonedConnection(t *testing.T) {
	_, e := executionFixture(t)
	count := func() int {
		t.Helper()
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := count()
	for i := 0; i < 100; i++ {
		f, err := e.Open(".", os.O_RDONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	if after := count(); after != before {
		t.Fatalf("descriptor leak: before=%d after=%d", before, after)
	}
	e.conn.Close()
	if _, err := e.Open(".", os.O_RDONLY, 0); err == nil {
		t.Fatal("poisoned channel accepted an operation")
	}
	select {
	case <-e.done:
	default:
		t.Fatal("failed exchange returned before reaping worker")
	}
}
func TestExecutionPacketRejectsExtraDescriptors(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	a, b := os.NewFile(uintptr(pair[0]), "a"), os.NewFile(uintptr(pair[1]), "b")
	defer a.Close()
	defer b.Close()
	sender, err := packetConn(a)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiver, err := packetConn(b)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	input, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	count := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := count()
	if _, _, err = sender.WriteMsgUnix([]byte(`{"op":"open"}`), unix.UnixRights(int(input.Fd()), int(input.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	var request executionRequest
	var reader packetReader
	if f, err := reader.read(receiver, &request); f != nil || !errors.Is(err, domain.ErrLimit) {
		t.Fatalf("extra descriptors: %v %v", f, err)
	}
	if after := count(); after != before {
		t.Fatalf("rejected packet leaked descriptors: %d to %d", before, after)
	}
	if err = sendPacket(sender, strings.Repeat("x", packetLimit), nil); !errors.Is(err, domain.ErrLimit) {
		t.Fatalf("oversized packet: %v", err)
	}
}

func TestExecutionDeniesSameUIDProcDescriptorAccess(t *testing.T) {
	_, worker := executionFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer binary.Close()
	probe := exec.Command("/proc/self/fd/3", "--filegate-proc-probe", fmt.Sprintf("/proc/%d/fd/4", worker.cmd.Process.Pid))
	probe.ExtraFiles = []*os.File{binary}
	probe.Env = []string{"GOMAXPROCS=1"}
	probe.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 12345, Gid: 12345, Groups: []uint32{12346}}}
	if output, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("same-UID probe: %v: %s", err, output)
	}
}

// Each timed operation reads every file once at the same path depth. One worker
// is reused for the whole benchmark scope, so growth measures per-file IPC and
// access enforcement, not repeated process startup or changing traversal depth.
func BenchmarkExecutionReadSweep(b *testing.B) {
	for _, entries := range []int{64, 256, 1024} {
		b.Run(fmt.Sprintf("entries=%d", entries), func(b *testing.B) {
			files, worker := executionFixture(b)
			if err := os.Mkdir(filepath.Join(files.root.Name(), "entries"), 0755); err != nil {
				b.Fatal(err)
			}
			paths := make([]string, entries)
			data := []byte("0123456789abcdef0123456789abcdef")
			for i := range paths {
				paths[i] = fmt.Sprintf("entries/%04d", i)
				if err := os.WriteFile(filepath.Join(files.root.Name(), paths[i]), data, 0644); err != nil {
					b.Fatal(err)
				}
			}
			buffer := make([]byte, len(data))
			b.ReportAllocs()
			b.SetBytes(int64(entries * len(data)))
			b.ResetTimer()
			for range b.N {
				for _, path := range paths {
					f, err := worker.Open(path, os.O_RDONLY, 0)
					if err != nil {
						b.Fatal(err)
					}
					_, readErr := io.ReadFull(f, buffer)
					closeErr := f.Close()
					if readErr != nil {
						b.Fatal(readErr)
					}
					if closeErr != nil {
						b.Fatal(closeErr)
					}
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*entries), "ns/file")
		})
	}
}

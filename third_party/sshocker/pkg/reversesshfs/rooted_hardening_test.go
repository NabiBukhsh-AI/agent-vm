//go:build linux || darwin || windows

package reversesshfs

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

type guardCall struct {
	rel   string
	entry bool
}

// A guest builds a guarded file's parent folder elsewhere and moves it into place: Guard is told
// that the names are being created, moved, linked or removed (entry), so it can refuse the
// parent folder of a guarded path, which a write-only check on the path would not see.
func TestRootedGuardParents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no symlinks here and asks with normalized names")
	}
	var calls []guardCall
	allow := false
	Guard = func(root, rel string, entry bool) bool {
		calls = append(calls, guardCall{rel, entry})
		parts := strings.Split(strings.ToLower(rel), "/")
		last := parts[len(parts)-1]
		if allow {
			return true
		}
		if last == ".vscode" && entry {
			return false
		}
		return !(last == "tasks.json" && len(parts) > 1 && parts[len(parts)-2] == ".vscode")
	}
	t.Cleanup(func() { Guard = nil })
	c, root := setupRooted(t, false)
	p := func(s string) string { return filepath.Join(root, s) }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(p(".vscode"), 0o755))
	must(os.WriteFile(p(".vscode/tasks.json"), []byte("orig"), 0o644))
	must(c.Mkdir(p("evil")))
	f, err := c.Create(p("evil/tasks.json"))
	must(err)
	_, _ = f.Write([]byte("pwn"))
	must(f.Close())
	if !slices.Contains(calls, guardCall{"evil/tasks.json", false}) {
		t.Errorf("a write is not an entry: %v", calls)
	}

	for _, op := range []struct {
		name string
		rel  string
		f    func() error
	}{
		{"rename onto the parent", ".vscode", func() error { return c.PosixRename(p("evil"), p(".vscode")) }},
		{"rename onto the parent, no replace", ".vscode", func() error { return c.Rename(p("evil"), p(".vscode")) }},
		{"symlink as the parent", ".vscode", func() error { return c.Symlink("evil", p(".vscode")) }},
		{"rename the parent away", ".vscode", func() error { return c.PosixRename(p(".vscode"), p("old")) }},
		{"remove the parent", ".vscode", func() error { return c.RemoveDirectory(p(".vscode")) }},
		{"hard link to the file", ".vscode/tasks.json", func() error { return c.Link(p(".vscode/tasks.json"), p("evil/alias")) }},
		{"mkdir the parent", "sub/.vscode", func() error { return c.Mkdir(p("sub/.vscode")) }},
	} {
		if op.rel == "sub/.vscode" {
			must(os.Mkdir(p("sub"), 0o755))
		}
		calls = nil
		if err := op.f(); err == nil {
			t.Errorf("%s: allowed though Guard said no", op.name)
		}
		if !slices.Contains(calls, guardCall{op.rel, true}) {
			t.Errorf("%s: Guard asked %v, not %q as an entry", op.name, calls, op.rel)
		}
	}
	if b, _ := os.ReadFile(p(".vscode/tasks.json")); string(b) != "orig" {
		t.Errorf("tasks.json: %q", b)
	}
	if fi, err := os.Lstat(p(".vscode")); err != nil || !fi.IsDir() {
		t.Errorf(".vscode: %v, %v", fi, err)
	}
	if _, err := os.Lstat(p("evil/alias")); err == nil {
		t.Error("hard link made")
	}

	// With Guard saying yes, the same moves work: they were refused by Guard alone.
	allow = true
	must(c.PosixRename(p(".vscode"), p("old")))
	must(c.PosixRename(p("evil"), p(".vscode")))
	if b, _ := os.ReadFile(p(".vscode/tasks.json")); string(b) != "pwn" {
		t.Errorf("tasks.json: %q", b)
	}
}

func TestFoldNameIgnorables(t *testing.T) {
	// Spellings that a casefold ext4, f2fs or tmpfs, or HFS+, takes as ".git".
	for _, r := range []rune{
		0x200b, 0x200c, 0x200d, 0x200e, 0x200f, 0x202a, 0x202e, 0x2060, 0x2061, 0x2064, 0x206a, 0x206f, 0xfeff,
		0xad, 0x34f, 0x61c, 0x115f, 0x1160, 0x17b4, 0x17b5, 0x180b, 0x180e, 0x180f, 0x3164, 0xfe00, 0xfe0f, 0xffa0,
		0xfff0, 0xe0001, 0xe0020, 0xe0100, 0xe01ef,
	} {
		s := string(r)
		for _, name := range []string{".g" + s + "it", ".gi" + s + "t", ".git" + s, s + ".git", ".GIT" + s} {
			if !sameName(name, ".git") {
				t.Errorf("%+q should match .git", name)
			}
		}
	}
	// Dotless i is "i" in NTFS's upcase table; long s folds to s.
	if !sameName(".gıt", ".git") || !sameName("ſrc", "src") {
		t.Error("dotless i or long s")
	}
	for _, name := range []string{".gt", ".gitt", ".gi.t", ".ǵit", ".g it"} {
		if sameName(name, ".git") {
			t.Errorf("%+q should not match .git", name)
		}
	}
}

func FuzzFoldIgnorables(f *testing.F) {
	for _, s := range []string{".git", ".g​it", "Aㅤb", "é", "é"} {
		f.Add(s, uint8(1))
	}
	f.Fuzz(func(t *testing.T, s string, at uint8) {
		// An ignorable code point anywhere changes nothing.
		i := int(at) % (len(s) + 1)
		for i > 0 && i < len(s) && s[i]&0xc0 == 0x80 {
			i--
		}
		if got, want := FoldName(s[:i]+"​️ㅤ"+s[i:]), FoldName(s); got != want {
			t.Errorf("FoldName(%q with ignorables at %d) = %q, want %q", s, i, got, want)
		}
	})
}

// An opened directory renamed, with a symlink to a folder outside the root put in its place,
// must not list the metadata of the files outside. (This holds because Go's File.Readdir stats
// relative to the directory; go.mod requires a Go that does.)
func TestRootedListNoEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no symlinks")
	}
	_, root, h := setupRootedHandlers(t, false)
	outside := filepath.Join(filepath.Dir(root), "outside")
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ path, content string }{
		{filepath.Join(root, "d", "a"), "x"},
		{filepath.Join(root, "d", "b"), "x"},
		{filepath.Join(outside, "a"), strings.Repeat("secret", 100)},
	} {
		if err := os.WriteFile(f.path, []byte(f.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l, err := h.Filelist(sftp.NewRequest("List", filepath.Join(root, "d")))
	if err != nil {
		t.Fatal(err)
	}
	defer l.(interface{ Close() error }).Close()
	if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "d2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	ls := make([]os.FileInfo, 10)
	n, _ := l.ListAt(ls, 0)
	for _, fi := range ls[:n] {
		if fi.Size() != 1 {
			t.Errorf("%s: size %d, the metadata of a file outside the root", fi.Name(), fi.Size())
		}
	}
}

// The setuid, setgid and sticky bits are never set by the client.
func TestRootedNoSpecialBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mode bits")
	}
	c, root, h := setupRootedHandlers(t, false)
	p := func(s string) string { return filepath.Join(root, s) }
	special := func(s string) os.FileMode {
		t.Helper()
		fi, err := os.Stat(p(s))
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	}
	const mode = 0o7755
	attrs := []byte{0, 0, byte(mode >> 8), byte(mode & 0xff)}

	r := sftp.NewRequest("Put", p("src/open"))
	r.Flags = 0x2 | 0x8 // write, creat
	r.Attrs = attrs
	f, err := h.openFile(r)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if m := special("src/open"); m != 0 {
		t.Errorf("open: %v", m)
	}

	if err := c.Chmod(p("src/main.go"), mode); err != nil {
		t.Fatal(err)
	}
	if m := special("src/main.go"); m != 0 {
		t.Errorf("chmod: %v", m)
	}

	hf, err := c.OpenFile(p("src/main.go"), os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if err := hf.Chmod(mode); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	if m := special("src/main.go"); m != 0 {
		t.Errorf("fchmod: %v", m)
	}

	// Mkdir takes its attributes through the sftp package; the handler applies them.
	mk := sftp.NewRequest("Mkdir", p("src/dir"))
	mk.Flags = 0x4 // permissions
	mk.Attrs = append(make([]byte, 0, 4), attrs...)
	if err := h.Filecmd(mk); err != nil {
		t.Fatal(err)
	}
	if m := special("src/dir"); m != 0 {
		t.Errorf("mkdir: %v", m)
	}
}

// A removal the server made for the guest is not relayed: the host's event for it must not
// leave a token that swallows the guest's next removal of a file it created again.
func TestRootedOwnRemovalNotRelayed(t *testing.T) {
	c, root, h := setupRootedHandlers(t, false)
	p := func(s string) string { return filepath.Join(root, s) }
	create := func(s string) {
		t.Helper()
		f, err := c.Create(p(s))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	create("f")
	if err := c.Remove(p("f")); err != nil {
		t.Fatal(err)
	}
	if h.expectRemove(p("f")) {
		t.Fatal("the host's event for the guest's own removal was relayed")
	}
	create("f")
	if err := c.Remove(p("f")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p("f")); err == nil {
		t.Fatal("rm f; touch f; rm f left the file on the host")
	}

	// A rename away is a removal as well: log rotation.
	create("log")
	if err := c.PosixRename(p("log"), p("log.1")); err != nil {
		t.Fatal(err)
	}
	create("log")
	if h.expectRemove(p("log")) {
		t.Fatal("the host's event for the guest's rename was relayed")
	}
	if err := c.Remove(p("log")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p("log")); err == nil {
		t.Fatal("the new log was not removed")
	}

	// Once the event window is over, a host deletion is relayed again.
	now := time.Now()
	h.now = func() time.Time { return now.Add(ownRemovalTTL + time.Second) }
	if !h.expectRemove(p("log")) {
		t.Error("not relayed after the window")
	}
}

// A removal that fails removed nothing: the host's deletion of the path that follows is still relayed.
func TestRootedFailedRemovalNotOwn(t *testing.T) {
	c, root, h := setupRootedHandlers(t, false)
	p := func(s string) string { return filepath.Join(root, s) }
	if err := os.MkdirAll(p("d/sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDirectory(p("d")); err == nil {
		t.Fatal("a folder with something in it was removed")
	}
	if !h.expectRemove(p("d")) {
		t.Error("a removal that failed hid the host's deletion")
	}
}

// A file the guest creates after the host deleted it, within the token's life, is its own.
func TestRootedTokenDroppedByCreate(t *testing.T) {
	c, root, h := setupRootedHandlers(t, false)
	p := func(s string) string { return filepath.Join(root, s) }
	if !h.expectRemove(p("new")) {
		t.Fatal("not expected")
	}
	f, err := c.Create(p("new"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := c.Remove(p("new")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p("new")); err == nil {
		t.Fatal("the removal was swallowed though the guest created the file")
	}

	// Same for the other ways to create a name.
	for name, create := range map[string]func(string) error{
		"mkdir":   func(s string) error { return c.Mkdir(s) },
		"symlink": func(s string) error { return c.Symlink("x", s) },
		"rename":  func(s string) error { return c.PosixRename(p("src/main.go"), s) },
	} {
		target := p("t-" + name)
		h.expectRemove(target)
		if err := create(target); err != nil {
			t.Fatal(name, err)
		}
		if err := c.Remove(target); err != nil {
			if err := c.RemoveDirectory(target); err != nil {
				t.Fatal(name, err)
			}
		}
		if _, err := os.Lstat(target); err == nil {
			t.Errorf("%s: the removal was swallowed", name)
		}
	}
}

// A host rm -rf of many files must not stall the server.
func TestExpectRemoveMany(t *testing.T) {
	_, root, h := setupRootedHandlers(t, false)
	start := time.Now()
	const n = 200000
	for i := 0; i < n; i++ {
		h.expectRemove(filepath.Join(root, "src", fmt.Sprint(i)))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("%d tokens took %v", n, d)
	}
	// They expire in order, and the tables empty out.
	now := time.Now()
	h.now = func() time.Time { return now.Add(noopRemovalTTL + time.Second) }
	h.expectRemove(filepath.Join(root, "x"))
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.relay.tokens.m) != 1 {
		t.Errorf("%d tokens left", len(h.relay.tokens.m))
	}
}

func TestExpiringKeyAddedAgain(t *testing.T) {
	e := newExpiring(10 * time.Second)
	t0 := time.Now()
	e.add("k", t0)
	e.add("k", t0.Add(8*time.Second))
	// The first entry expired, the key is still there with the later expiry.
	if !e.has("k", t0.Add(12*time.Second)) {
		t.Error("key added again was purged by its first entry")
	}
	if e.has("k", t0.Add(19*time.Second)) {
		t.Error("key not expired")
	}
	if len(e.m) != 0 {
		t.Error("left over")
	}
}

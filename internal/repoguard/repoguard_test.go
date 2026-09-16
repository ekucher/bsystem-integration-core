package repoguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A compiled binary was committed to this repository's root and nothing
// noticed. It was 13 MB, it shipped in every clone and every Docker build
// context, and it stayed for as long as it did because reviewing a diff is how
// this class of mistake is normally caught — and a binary blob is the one diff
// nobody reads.
//
// The cause is worth stating, because the fix people reach for does not
// address it. `go build ./cmd/mapping-audit` with no -o writes its output to
// the repository root, named after the command. .gitignore listed `bin/` and
// `dist/` — the directories a build could be *told* to use — and not the place
// the toolchain actually writes by default. bsystem-deploy has the same scar:
// a `loadtest/loadtest` line added after the same thing happened there. Naming
// one more path each time it happens is not a guard, it is a record of the
// times somebody noticed.
//
// So this checks the property rather than the paths: nothing tracked in Git
// may be a compiled executable or an archive.

// magic identifies a file class by its leading bytes. Content, not extension:
// a stray binary does not arrive with a helpful suffix, which is exactly how
// the one that prompted this went unnoticed.
var magic = []struct {
	name   string
	prefix []byte
}{
	{"an ELF executable", []byte{0x7f, 'E', 'L', 'F'}},
	{"a Windows executable", []byte{'M', 'Z'}},
	{"a Mach-O executable", []byte{0xfe, 0xed, 0xfa, 0xce}},
	{"a Mach-O executable", []byte{0xfe, 0xed, 0xfa, 0xcf}},
	{"a Mach-O executable", []byte{0xcf, 0xfa, 0xed, 0xfe}},
	{"a Mach-O universal binary", []byte{0xca, 0xfe, 0xba, 0xbe}},
	{"a zip or jar archive", []byte{'P', 'K', 0x03, 0x04}},
	{"a gzip archive", []byte{0x1f, 0x8b}},
	{"an xz archive", []byte{0xfd, '7', 'z', 'X', 'Z'}},
	{"a zstd archive", []byte{0x28, 0xb5, 0x2f, 0xfd}},
}

// Files that are legitimately binary would be listed here, with the reason.
// The list is empty: this repository tracks no binary file, and an addition
// should have to argue for itself in review rather than slip in under a
// pattern.
var allowed = map[string]string{}

func trackedFiles(t *testing.T) (root string, names []string) {
	t.Helper()

	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		// Not skipped. A check that quietly does nothing outside a git
		// checkout is a check that quietly does nothing in CI the day the
		// checkout changes shape, and this suite has been bitten by a guard
		// that never ran.
		t.Fatalf("cannot locate the repository root: %v", err)
	}
	root = strings.TrimSpace(string(top))

	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("cannot list tracked files: %v", err)
	}
	for _, name := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return root, names
}

func TestNoCompiledArtefactIsTracked(t *testing.T) {
	root, names := trackedFiles(t)

	// Without this the loop below passes on an empty list, which is how a
	// broken invocation reads as a clean repository.
	if len(names) < 2 {
		t.Fatalf("git reported %d tracked files; the check would pass vacuously", len(names))
	}

	examined := 0
	for _, name := range names {
		if _, ok := allowed[name]; ok {
			continue
		}
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			// A path in the index with no regular file behind it is a
			// submodule, a symlink or a deletion staged elsewhere. Not this
			// test's business.
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			t.Errorf("cannot read tracked file %s: %v", name, err)
			continue
		}
		head := make([]byte, 8)
		n, _ := file.Read(head)
		file.Close()
		head = head[:n]
		examined++

		for _, kind := range magic {
			if len(head) >= len(kind.prefix) && string(head[:len(kind.prefix)]) == string(kind.prefix) {
				t.Errorf("%s is tracked in Git and is %s; build output belongs in .gitignore, and a binary that must be tracked belongs in the allowed list with its reason", name, kind.name)
				break
			}
		}
	}

	if examined == 0 {
		t.Fatal("no tracked file was examined; the check proves nothing")
	}
}

// The default output path is the one that actually gets used, so it is the one
// that has to be covered. `go build ./cmd/x` writes ./x.
func TestTheDefaultBuildOutputOfEveryCommandIsIgnored(t *testing.T) {
	root, _ := trackedFiles(t)

	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatalf("read cmd directory: %v", err)
	}
	commands := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			commands = append(commands, entry.Name())
		}
	}
	if len(commands) == 0 {
		t.Fatal("no commands found; this check would pass vacuously")
	}

	for _, command := range commands {
		// check-ignore answers the question as git itself would, rather than
		// by reading .gitignore and reimplementing its precedence rules.
		err := exec.Command("git", "-C", root, "check-ignore", "-q", command).Run()
		if err != nil {
			t.Errorf("`go build ./cmd/%s` writes ./%s, which git does not ignore; committing it is then one `git add -A` away", command, command)
		}
	}
}

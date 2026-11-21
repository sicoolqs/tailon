package logquery

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultGroup is the group name Tailon uses for file specs without a group.
const DefaultGroup = "__default__"

// Spec is a copy of a Tailon file spec. The MCP layer receives a function that
// returns the current specs, so newly rotated files are discovered on demand.
type Spec struct {
	Path  string
	Type  string
	Alias string
	Group string
}

// Source returns the currently configured file specs.
type Source func() []Spec

// FileRef is a fully resolved log file reference. Path is absolute and is
// never exposed to MCP clients; only FileID and Name are public.
type FileRef struct {
	ID       string
	Group    string
	Name     string
	Alias    string
	Path     string
	Exists   bool
	Size     int64
	ModTime  int64
	Readable bool
	root     string
}

// Registry discovers configured log files and enforces the Tailon file scope.
type Registry struct {
	source Source
}

// NewRegistry creates a registry backed by source.
func NewRegistry(source Source) *Registry {
	return &Registry{source: source}
}

// Groups returns group names and their current file counts, matching the
// listing semantics used by the web UI.
func (r *Registry) Groups() map[string]int {
	out := make(map[string]int)
	for _, ref := range r.List() {
		out[ref.Group]++
	}
	return out
}

// List resolves every configured file spec. Globs are re-evaluated on each
// call, which is how rotated and newly created files become visible. The
// result is deterministic and sorted by group and display name.
func (r *Registry) List() []FileRef {
	if r == nil || r.source == nil {
		return nil
	}

	seen := make(map[string]bool)
	var refs []FileRef

	for _, spec := range r.source() {
		group := spec.Group
		if group == "" {
			group = DefaultGroup
		}
		root := specRoot(spec)
		switch spec.Type {
		case "glob":
			matches, err := filepath.Glob(spec.Path)
			if err != nil {
				continue
			}
			for _, match := range matches {
				ref := makeFileRef(spec, group, root, match)
				if ref.ID == "" || seen[ref.Group+"\x00"+ref.Path] {
					continue
				}
				seen[ref.Group+"\x00"+ref.Path] = true
				refs = append(refs, ref)
			}
		default:
			ref := makeFileRef(spec, group, root, spec.Path)
			if ref.ID == "" || seen[ref.Group+"\x00"+ref.Path] {
				continue
			}
			seen[ref.Group+"\x00"+ref.Path] = true
			refs = append(refs, ref)
		}
	}

	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Group != refs[j].Group {
			return refs[i].Group < refs[j].Group
		}
		if refs[i].Name != refs[j].Name {
			return refs[i].Name < refs[j].Name
		}
		return refs[i].Path < refs[j].Path
	})
	return refs
}

// Resolve re-validates a file ID against the current configuration. Callers
// must never trust a previously resolved path across requests.
func (r *Registry) Resolve(fileID string) (FileRef, bool) {
	if fileID == "" {
		return FileRef{}, false
	}
	for _, ref := range r.List() {
		if ref.ID == fileID {
			return ref, true
		}
	}
	return FileRef{}, false
}

// OpenedFile is a validated, open log file descriptor.
type OpenedFile struct {
	Ref        FileRef
	File       *os.File
	Info       os.FileInfo
	Identity   string
	Resolved   string
	Compressed bool
	Seekable   bool
}

// Close releases the descriptor.
func (o *OpenedFile) Close() error {
	if o == nil || o.File == nil {
		return nil
	}
	return o.File.Close()
}

// Open validates scope and file type, then opens the file. The file is
// re-resolved from its ID so a stale ID can never escape a changed scope.
func (r *Registry) Open(fileID string, supportGzip bool) (*OpenedFile, error) {
	ref, ok := r.Resolve(fileID)
	if !ok {
		return nil, NewError(CodeFileNotFound, "the requested file_id is not available").
			WithHint(false, "Call logs_list to discover current file identifiers.")
	}

	// Resolve symlinks first and enforce that the final target remains inside
	// the directory tree covered by the configured file spec.
	resolved, err := filepath.EvalSymlinks(ref.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NewError(CodeFileNotFound, "the requested log file no longer exists").
				WithHint(false, "Call logs_list again to rediscover available files.")
		}
		return nil, NewError(CodeFileAccessDenied, "the log file could not be resolved safely")
	}
	resolved = filepath.Clean(resolved)
	if !withinRoot(ref.root, resolved) {
		return nil, NewError(CodeFileAccessDenied, "the log file resolves outside the configured scope")
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, NewError(CodeFileNotFound, "the requested log file is unavailable")
	}
	if !info.Mode().IsRegular() {
		return nil, NewError(CodeUnsupportedType, "only regular files can be read").
			WithHint(false, "Devices, pipes and sockets are rejected.")
	}

	f, err := os.Open(resolved)
	if err != nil {
		return nil, NewError(CodeFileAccessDenied, "the log file could not be opened")
	}

	openedInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, NewError(CodeInternalError, "the log file could not be inspected")
	}
	if !openedInfo.Mode().IsRegular() {
		_ = f.Close()
		return nil, NewError(CodeUnsupportedType, "only regular files can be read")
	}

	// Guard against a file replacement between validation and open.
	currentInfo, err := os.Stat(resolved)
	if err != nil || !os.SameFile(openedInfo, currentInfo) {
		_ = f.Close()
		return nil, NewError(CodeFileChanged, "the log file changed while it was being opened").
			WithHint(true, "Retry the query.")
	}
	currentResolved, err := filepath.EvalSymlinks(ref.Path)
	if err != nil || filepath.Clean(currentResolved) != resolved {
		_ = f.Close()
		return nil, NewError(CodeFileChanged, "the log file was replaced or rotated").
			WithHint(false, "Call logs_list again to rediscover available files.")
	}

	compressed := isCompressedFile(resolved, f)
	if compressed && !supportGzip {
		_ = f.Close()
		return nil, NewError(CodeUnsupportedType, "compressed log files are disabled on this server")
	}

	return &OpenedFile{
		Ref:        ref,
		File:       f,
		Info:       openedInfo,
		Identity:   fileIdentity(openedInfo),
		Resolved:   resolved,
		Compressed: compressed,
		Seekable:   !compressed,
	}, nil
}

// Revalidate checks that an open file still refers to the same on-disk object
// and has not been truncated since the query snapshot was taken.
func (o *OpenedFile) Revalidate(snapshotSize int64) error {
	if o == nil || o.File == nil {
		return NewError(CodeFileChanged, "the log file is no longer open")
	}
	info, err := o.File.Stat()
	if err != nil {
		return NewError(CodeFileChanged, "the log file could not be re-inspected")
	}
	current, err := os.Stat(o.Resolved)
	if err != nil || !os.SameFile(info, current) || fileIdentity(info) != o.Identity {
		return NewError(CodeFileRotated, "the log file was rotated or replaced").
			WithHint(false, "Call logs_list again and restart the query.")
	}
	if snapshotSize > 0 && info.Size() < snapshotSize {
		return NewError(CodeFileChanged, "the log file was truncated during the query").
			WithHint(false, "Restart the query with the current file contents.")
	}
	return nil
}

func makeFileRef(spec Spec, group, root, path string) FileRef {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	abs = filepath.Clean(abs)

	name := spec.Alias
	if name != "" && spec.Type == "glob" {
		name = filepath.Join(spec.Alias, filepath.Base(abs))
	}
	if name == "" {
		name = filepath.Base(abs)
	}

	ref := FileRef{
		ID:    fileID(group, abs),
		Group: group,
		Name:  name,
		Alias: spec.Alias,
		Path:  abs,
		root:  root,
	}
	if info, err := os.Stat(abs); err == nil {
		ref.Exists = true
		ref.Size = info.Size()
		ref.ModTime = info.ModTime().Unix()
		ref.Readable = info.Mode().IsRegular()
	}
	return ref
}

// fileID is stable for as long as the same absolute path belongs to the same
// group. Hashing prevents collisions across groups and keeps paths private.
func fileID(group, absPath string) string {
	sum := sha256.Sum256([]byte(group + "\x00" + absPath))
	return "f_" + slug(group) + "_" + hex.EncodeToString(sum[:])[:10]
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "default"
	}
	if len(out) > 24 {
		out = out[:24]
	}
	return out
}

// specRoot returns the directory tree that a spec is allowed to read. For a
// single file this is its parent directory; for a glob it is the longest
// wildcard-free prefix directory. Symlinks must resolve inside this tree.
func specRoot(spec Spec) string {
	pattern := spec.Path
	if spec.Type == "glob" {
		if idx := strings.IndexAny(pattern, "*?["); idx >= 0 {
			pattern = pattern[:idx]
			if !strings.HasSuffix(pattern, string(os.PathSeparator)) && !strings.HasSuffix(pattern, "/") {
				pattern = filepath.Dir(pattern)
			}
		}
	}
	root, err := filepath.Abs(pattern)
	if err != nil {
		root = filepath.Clean(pattern)
	}
	if spec.Type != "glob" {
		root = filepath.Dir(root)
	}
	root = filepath.Clean(root)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return root
}

func withinRoot(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func isCompressedFile(path string, f *os.File) bool {
	if strings.HasSuffix(strings.ToLower(path), ".gz") || strings.HasSuffix(strings.ToLower(path), ".gzip") {
		return true
	}
	var magic [2]byte
	if _, err := f.ReadAt(magic[:], 0); err == nil {
		return magic[0] == 0x1f && magic[1] == 0x8b
	}
	return false
}

func (r FileRef) String() string {
	return fmt.Sprintf("%s(%s)", r.Name, r.ID)
}

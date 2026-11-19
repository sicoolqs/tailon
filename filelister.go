package main

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ListEntry is an entry that appears in the UI file input.
// FileSpecs are transformed into one or more ListEntry instances, which the backend sends to the client.
type ListEntry struct {
	Path    string    `json:"path"`
	Alias   string    `json:"alias"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	Exists  bool      `json:"exists"`
}

func fileInfo(path string) *ListEntry {
	entry := ListEntry{}
	entry.Path = path

	info, err := os.Stat(path)
	if err == nil {
		entry.Exists = true
		entry.Size = info.Size()
		entry.ModTime = info.ModTime()
	}

	return &entry
}

var (
	allFilesMu sync.RWMutex
	allFiles   map[string]bool
)

func createListing(filespecs []FileSpec) map[string][]*ListEntry {
	files := make(map[string]bool)
	res := make(map[string][]*ListEntry)

	for _, spec := range filespecs {
		group := "__default__"
		if spec.Group != "" {
			group = spec.Group
		}

		switch spec.Type {
		case "file":
			entry := fileInfo(spec.Path)
			if spec.Alias != "" {
				entry.Alias = spec.Alias
			} else {
				entry.Alias = entry.Path
			}
			res[group] = append(res[group], entry)
			files[entry.Path] = true
		case "glob":
			matches, _ := filepath.Glob(spec.Path)
			for _, match := range matches {
				entry := fileInfo(match)
				if spec.Alias != "" {
					entry.Alias = filepath.Join(spec.Alias, filepath.Base(entry.Path))
				} else {
					cwd, _ := os.Getwd()
					rel, _ := filepath.Rel(cwd, entry.Path)
					entry.Alias = rel
				}
				res[group] = append(res[group], entry)
				files[entry.Path] = true
			}
		}
	}

	// Swap the complete map under the lock. Readers can then use the old
	// snapshot without observing a partially built map or a concurrent write.
	allFilesMu.Lock()
	allFiles = files
	allFilesMu.Unlock()

	return res
}

func fileAllowed(path string) bool {
	allFilesMu.RLock()
	_, ok := allFiles[path]
	allFilesMu.RUnlock()
	return ok
}

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// replaceFiles stages the complete pin on the destination filesystem.
// Multiwriter's owned src directory is replaced as a unit; other outputs are files.
// The rename argument permits deterministic replacement and rollback failure tests.
// It requires exclusive access to the destinations; this is not crash-atomic.
func replaceFiles(dir string, files map[string][]byte, rename func(string, string) error, verify func() error) (err error) {
	sourceDirectory := multiwriterDirectory + "src"
	names := make([]string, 0, len(files))
	var sources []string
	for name := range files {
		if !filepath.IsLocal(name) || name == "." || filepath.Clean(name) != filepath.FromSlash(name) || strings.Contains(name, "\\") {
			return fmt.Errorf("invalid output path %q", name)
		}
		for parent := name; ; {
			slash := strings.LastIndexByte(parent, '/')
			if slash < 0 {
				break
			}
			parent = parent[:slash]
			if _, exists := files[parent]; exists {
				return fmt.Errorf("output path %s is also a parent of %s", parent, name)
			}
		}
		if strings.HasPrefix(name, sourceDirectory+"/") {
			if len(sources) == 0 {
				names = append(names, sourceDirectory)
			}
			sources = append(sources, name)
		} else {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect output directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("output directory %s is not a directory", dir)
	}
	outputs := make([]struct {
		path, staged, backup string
		mode                 os.FileMode
		exists, backedUp     bool
		installed, tree      bool
	}, len(names))
	var missingDirs, createdDirs []string
	seenDirs := make(map[string]bool)
	for i, name := range names {
		parent := dir
		parts := strings.Split(name, "/")
		for _, part := range parts[:len(parts)-1] {
			parent = filepath.Join(parent, part)
			info, err := os.Lstat(parent)
			if os.IsNotExist(err) {
				if !seenDirs[parent] {
					seenDirs[parent] = true
					missingDirs = append(missingDirs, parent)
				}
				continue
			}
			if err != nil {
				return fmt.Errorf("inspect output directory %s: %w", parent, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("output directory %s is not a directory", parent)
			}
		}
		out := &outputs[i]
		out.path = filepath.Join(dir, name)
		out.tree = name == sourceDirectory && len(sources) != 0
		out.mode = 0644
		info, statErr := os.Lstat(out.path)
		switch {
		case os.IsNotExist(statErr):
		case statErr != nil:
			return fmt.Errorf("inspect %s: %w", out.path, statErr)
		case out.tree && !info.IsDir():
			return fmt.Errorf("replace %s: destination is not a directory", out.path)
		case !out.tree && !info.Mode().IsRegular():
			return fmt.Errorf("replace %s: destination is not a regular file", out.path)
		default:
			out.exists = true
			if !out.tree {
				out.mode = info.Mode().Perm()
			}
		}
	}
	stage, err := os.MkdirTemp(dir, ".deps-update-")
	if err != nil {
		return fmt.Errorf("create updater staging directory: %w", err)
	}
	preserve := false
	defer func() {
		if !preserve {
			if cleanupErr := os.RemoveAll(stage); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove updater staging directory %s: %w", stage, cleanupErr))
			}
		}
	}()
	for i, name := range names {
		out := &outputs[i]
		out.staged = filepath.Join(stage, name+".new")
		out.backup = filepath.Join(stage, name+".old")
		if err := os.MkdirAll(filepath.Dir(out.staged), 0755); err != nil {
			return fmt.Errorf("create staging directory: %w", err)
		}
		if out.tree {
			if err := os.Mkdir(out.staged, 0755); err != nil {
				return fmt.Errorf("create staged source directory: %w", err)
			}
			for _, source := range sources {
				target := filepath.Join(out.staged, strings.TrimPrefix(source, sourceDirectory+"/"))
				if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
					return fmt.Errorf("create staged source directory: %w", err)
				}
				if err := os.WriteFile(target, files[source], 0644); err != nil {
					return fmt.Errorf("stage %s: %w", source, err)
				}
			}
		} else if err := os.WriteFile(out.staged, files[name], out.mode); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
		if out.exists && !out.tree {
			if err := os.Chmod(out.staged, out.mode); err != nil {
				return fmt.Errorf("preserve %s permissions: %w", name, err)
			}
		}
	}
	rollback := func(cause error) error {
		var restoreErr error
		for i := len(outputs) - 1; i >= 0; i-- {
			out := &outputs[i]
			if out.installed {
				remove := os.Remove
				if out.tree {
					remove = os.RemoveAll
				}
				if err := remove(out.path); err != nil && !os.IsNotExist(err) {
					restoreErr = errors.Join(restoreErr, fmt.Errorf("remove replacement %s: %w", out.path, err))
					continue
				}
			}
			if out.backedUp {
				if err := rename(out.backup, out.path); err != nil {
					restoreErr = errors.Join(restoreErr, fmt.Errorf("restore %s: %w", out.path, err))
				}
			}
		}
		for _, createdDir := range slices.Backward(createdDirs) {
			if err := os.Remove(createdDir); err != nil && !os.IsNotExist(err) {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("remove new directory %s: %w", createdDir, err))
			}
		}
		if restoreErr != nil {
			preserve = true
			return errors.Join(cause, fmt.Errorf("rollback incomplete; recovery files retained in %s: %w", stage, restoreErr))
		}
		return cause
	}
	for _, parent := range missingDirs {
		if err := os.Mkdir(parent, 0755); err != nil {
			return rollback(fmt.Errorf("create output directory %s: %w", parent, err))
		}
		createdDirs = append(createdDirs, parent)
	}
	for i := range outputs {
		out := &outputs[i]
		if out.exists {
			if err := rename(out.path, out.backup); err != nil {
				return rollback(fmt.Errorf("save previous %s: %w", out.path, err))
			}
			out.backedUp = true
		}
		if err := rename(out.staged, out.path); err != nil {
			return rollback(fmt.Errorf("install %s: %w", out.path, err))
		}
		out.installed = true
	}
	if err := verify(); err != nil {
		return rollback(fmt.Errorf("verify updated dependencies: %w", err))
	}
	return nil
}

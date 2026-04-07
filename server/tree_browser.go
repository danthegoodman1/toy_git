package server

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

type CommitTreeEntry struct {
	Name  string
	Path  string
	Mode  filemode.FileMode
	Hash  plumbing.Hash
	IsDir bool
}

// WriteCommitTree streams the full file tree for a commit using Git tree
// objects from the object database.
func WriteCommitTree(w io.Writer, s storer.EncodedObjectStorer, commitHash plumbing.Hash) error {
	root, err := getCommitRootTree(s, commitHash)
	if err != nil {
		return err
	}

	buffered := bufio.NewWriter(w)
	if err := writeTree(buffered, root, 0); err != nil {
		return err
	}

	return buffered.Flush()
}

// FormatCommitTree renders the full file tree for a commit using Git tree
// objects from the object database.
func FormatCommitTree(s storer.EncodedObjectStorer, commitHash plumbing.Hash) (string, error) {
	var buf bytes.Buffer
	if err := WriteCommitTree(&buf, s, commitHash); err != nil {
		return "", err
	}

	return buf.String(), nil
}

// ListCommitTreePath lists the immediate entries for a directory path within a commit.
// An empty path, "." and "/" all mean the root tree.
func ListCommitTreePath(s storer.EncodedObjectStorer, commitHash plumbing.Hash, dir string) ([]CommitTreeEntry, error) {
	root, err := getCommitRootTree(s, commitHash)
	if err != nil {
		return nil, err
	}

	tree, cleanPath, err := getTreeAtPath(root, dir)
	if err != nil {
		return nil, err
	}

	order := make([]int, len(tree.Entries))
	for i := range tree.Entries {
		order[i] = i
	}
	sortTreeEntries(tree.Entries, order)

	entries := make([]CommitTreeEntry, 0, len(order))
	for _, idx := range order {
		entry := tree.Entries[idx]
		entryPath := entry.Name
		if cleanPath != "" {
			entryPath = cleanPath + "/" + entry.Name
		}

		entries = append(entries, CommitTreeEntry{
			Name:  entry.Name,
			Path:  entryPath,
			Mode:  entry.Mode,
			Hash:  entry.Hash,
			IsDir: entry.Mode == filemode.Dir,
		})
	}

	return entries, nil
}

// ReadCommitFile opens a reader for a file path within a commit.
func ReadCommitFile(s storer.EncodedObjectStorer, commitHash plumbing.Hash, filePath string) (io.ReadCloser, error) {
	root, err := getCommitRootTree(s, commitHash)
	if err != nil {
		return nil, err
	}

	cleanPath, err := cleanCommitPath(filePath)
	if err != nil {
		return nil, err
	}

	file, err := root.File(cleanPath)
	if err != nil {
		return nil, err
	}

	return file.Reader()
}

func writeTree(w *bufio.Writer, tree *object.Tree, depth int) error {
	order := make([]int, len(tree.Entries))
	for i := range tree.Entries {
		order[i] = i
	}

	sortTreeEntries(tree.Entries, order)

	for _, idx := range order {
		entry := tree.Entries[idx]
		writeIndent(w, depth)

		if entry.Mode == filemode.Dir {
			if _, err := fmt.Fprintf(w, "%s/\n", entry.Name); err != nil {
				return err
			}

			child, err := tree.Tree(entry.Name)
			if err != nil {
				return err
			}
			if err := writeTree(w, child, depth+1); err != nil {
				return err
			}
			continue
		}

		if _, err := fmt.Fprintf(w, "%s\n", entry.Name); err != nil {
			return err
		}
	}

	return nil
}

func writeIndent(w *bufio.Writer, depth int) {
	for range depth {
		_, _ = w.WriteString("  ")
	}
}

func getCommitRootTree(s storer.EncodedObjectStorer, commitHash plumbing.Hash) (*object.Tree, error) {
	commit, err := object.GetCommit(s, commitHash)
	if err != nil {
		return nil, err
	}

	return commit.Tree()
}

func getTreeAtPath(root *object.Tree, dir string) (*object.Tree, string, error) {
	cleanPath, err := cleanCommitPath(dir)
	if err != nil {
		return nil, "", err
	}
	if cleanPath == "" {
		return root, "", nil
	}
	tree, err := root.Tree(cleanPath)
	if err != nil {
		return nil, "", err
	}

	return tree, cleanPath, nil
}

func cleanCommitPath(raw string) (string, error) {
	cleanPath := strings.TrimPrefix(strings.TrimSpace(raw), "/")
	if cleanPath == "" || cleanPath == "." {
		return "", nil
	}

	cleanPath = path.Clean(cleanPath)
	if cleanPath == "." {
		return "", nil
	}
	if cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
		return "", fmt.Errorf("invalid path %q", raw)
	}

	return cleanPath, nil
}

func sortTreeEntries(entries []object.TreeEntry, order []int) {
	sort.Slice(order, func(i, j int) bool {
		left := entries[order[i]]
		right := entries[order[j]]

		leftDir := left.Mode == filemode.Dir
		rightDir := right.Mode == filemode.Dir
		if leftDir != rightDir {
			return leftDir
		}

		return left.Name < right.Name
	})
}

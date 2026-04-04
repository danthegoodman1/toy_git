package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage"
)

var ErrInvalidRepoPath = errors.New("invalid repository path")

type repoBackend interface {
	Open(ctx context.Context, repo string) (storage.Storer, error)
	LFSObjectPath(ctx context.Context, repo, oid string) (string, error)
	WriteLFSObject(ctx context.Context, repo, oid string, src io.Reader) error
}

type diskBackend struct {
	root string
}

var _ repoBackend = (*diskBackend)(nil)
var _ transport.Loader = (*diskBackend)(nil)

func newDiskBackend(root string) (*diskBackend, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve data dir: %w", err)
	}

	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	return &diskBackend{root: absRoot}, nil
}

func (b *diskBackend) Load(ep *transport.Endpoint) (storage.Storer, error) {
	return b.Open(context.Background(), ep.Path)
}

func (b *diskBackend) Open(ctx context.Context, repo string) (storage.Storer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	repoDir, err := b.repoDir(repo)
	if err != nil {
		return nil, err
	}

	if err := ensureBareRepoExists(repoDir); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return newDiskStorer(ctx, repoDir), nil
}

func (b *diskBackend) LFSObjectPath(ctx context.Context, repo, oid string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validateLFSOID(oid); err != nil {
		return "", err
	}

	repoDir, err := b.repoDir(repo)
	if err != nil {
		return "", err
	}
	if err := ensureBareRepoExists(repoDir); err != nil {
		return "", err
	}

	return filepath.Join(repoDir, "lfs", "objects", oid[:2], oid[2:4], oid), nil
}

func (b *diskBackend) WriteLFSObject(ctx context.Context, repo, oid string, src io.Reader) error {
	path, err := b.LFSObjectPath(ctx, repo, oid)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "lfs-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tmpName, path)
}

func (b *diskBackend) InitBareRepo(repo string) error {
	repoDir, err := b.repoDir(repo)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		return fmt.Errorf("create repo parent: %w", err)
	}

	if _, err := os.Stat(repoDir); err == nil {
		if err := ensureBareRepoExists(repoDir); err != nil {
			return err
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat repo dir: %w", err)
	}

	storer := newDiskStorer(context.Background(), repoDir)
	if err := storer.Init(); err != nil {
		return fmt.Errorf("init bare repo: %w", err)
	}

	return nil
}

func (b *diskBackend) repoDir(repo string) (string, error) {
	normalized, err := normalizeRepoPath(repo)
	if err != nil {
		return "", err
	}

	full := filepath.Clean(filepath.Join(b.root, filepath.FromSlash(normalized)))
	rel, err := filepath.Rel(b.root, full)
	if err != nil {
		return "", fmt.Errorf("resolve repo path: %w", err)
	}
	if rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrInvalidRepoPath
	}

	return full, nil
}

func InitBareRepo(dataDir, repo string) error {
	backend, err := newDiskBackend(dataDir)
	if err != nil {
		return err
	}

	return backend.InitBareRepo(repo)
}

func normalizeRepoPath(repo string) (string, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "", ErrInvalidRepoPath
	}
	if strings.Contains(repo, "\x00") || strings.Contains(repo, "\\") {
		return "", ErrInvalidRepoPath
	}

	repo = strings.TrimPrefix(repo, "/")
	repo = path.Clean(repo)
	repo = strings.TrimPrefix(repo, "/")
	if repo == "" || repo == "." || repo == ".." || strings.HasPrefix(repo, "../") {
		return "", ErrInvalidRepoPath
	}

	return repo, nil
}

func ensureBareRepoExists(repoDir string) error {
	info, err := os.Stat(repoDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return transport.ErrRepositoryNotFound
		}
		return fmt.Errorf("stat repo dir: %w", err)
	}
	if !info.IsDir() {
		return transport.ErrRepositoryNotFound
	}

	headPath := filepath.Join(repoDir, "HEAD")
	if _, err := os.Stat(headPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return transport.ErrRepositoryNotFound
		}
		return fmt.Errorf("stat repo HEAD: %w", err)
	}

	return nil
}

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

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
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
	root          string
	onlyBranch    plumbing.ReferenceName
	linearHistory bool
}

var _ repoBackend = (*diskBackend)(nil)
var _ transport.Loader = (*diskBackend)(nil)

func newDiskBackend(root string, onlyBranch string, linearHistory bool) (*diskBackend, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve data dir: %w", err)
	}

	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	return &diskBackend{
		root:          absRoot,
		onlyBranch:    plumbing.ReferenceName(onlyBranch),
		linearHistory: linearHistory,
	}, nil
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
	st := newDiskStorer(ctx, repoDir)
	if b.onlyBranch == "" && !b.linearHistory {
		return st, nil
	}

	return &policyStorer{
		Storer:        st,
		onlyBranch:    b.onlyBranch,
		linearHistory: b.linearHistory,
	}, nil
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
	backend, err := newDiskBackend(dataDir, "", false)
	if err != nil {
		return err
	}

	return backend.InitBareRepo(repo)
}

type policyStorer struct {
	storage.Storer
	onlyBranch    plumbing.ReferenceName
	linearHistory bool
}

func (s *policyStorer) SetReference(ref *plumbing.Reference) error {
	if err := s.checkReferenceAllowed(ref.Name()); err != nil {
		return err
	}
	if err := s.checkLinearHistory(ref); err != nil {
		return err
	}

	return s.Storer.SetReference(ref)
}

func (s *policyStorer) CheckAndSetReference(newRef, old *plumbing.Reference) error {
	if err := s.checkReferenceAllowed(newRef.Name()); err != nil {
		return err
	}
	if err := s.checkLinearHistory(newRef); err != nil {
		return err
	}

	return s.Storer.CheckAndSetReference(newRef, old)
}

func (s *policyStorer) RemoveReference(name plumbing.ReferenceName) error {
	if err := s.checkReferenceAllowed(name); err != nil {
		return err
	}

	return s.Storer.RemoveReference(name)
}

func (s *policyStorer) checkReferenceAllowed(name plumbing.ReferenceName) error {
	if s.onlyBranch == "" || name == plumbing.HEAD || name == s.onlyBranch {
		return nil
	}

	return fmt.Errorf("pushes are only allowed to %s", s.onlyBranch)
}

func (s *policyStorer) checkLinearHistory(ref *plumbing.Reference) error {
	if !s.linearHistory || ref == nil || ref.Name() == plumbing.HEAD || !ref.Name().IsBranch() || ref.Type() != plumbing.HashReference {
		return nil
	}

	current, err := s.Storer.Reference(ref.Name())
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil
		}
		return err
	}

	if current.Type() != plumbing.HashReference || current.Hash() == ref.Hash() {
		return nil
	}

	currentCommit, err := object.GetCommit(s.Storer, current.Hash())
	if err != nil {
		return err
	}
	newCommit, err := object.GetCommit(s.Storer, ref.Hash())
	if err != nil {
		return err
	}

	isAncestor, err := currentCommit.IsAncestor(newCommit)
	if err != nil {
		return err
	}
	if isAncestor {
		return nil
	}

	return fmt.Errorf("linear history required for %s", ref.Name())
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

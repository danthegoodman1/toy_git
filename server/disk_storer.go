package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	formatindex "github.com/go-git/go-git/v6/plumbing/format/index"
	"github.com/go-git/go-git/v6/plumbing/format/objfile"
	plumbinghash "github.com/go-git/go-git/v6/plumbing/hash"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage"
)

type diskStorer struct {
	root string
	ctx  context.Context
}

var _ storage.Storer = (*diskStorer)(nil)

func newDiskStorer(ctx context.Context, root string) *diskStorer {
	if ctx == nil {
		ctx = context.Background()
	}

	return &diskStorer{root: root, ctx: ctx}
}

func (s *diskStorer) Init() error {
	if err := s.checkContext(); err != nil {
		return err
	}

	for _, dir := range []string{
		filepath.Join(s.root, "objects"),
		filepath.Join(s.root, "objects", "tmp"),
		filepath.Join(s.root, "refs"),
		filepath.Join(s.root, "refs", "heads"),
		filepath.Join(s.root, "refs", "tags"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create repo dir %s: %w", dir, err)
		}
	}

	if _, err := os.Stat(filepath.Join(s.root, "HEAD")); errors.Is(err, os.ErrNotExist) {
		head := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.ReferenceName("refs/heads/master"))
		if err := s.SetReference(head); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("stat HEAD: %w", err)
	}

	if _, err := os.Stat(filepath.Join(s.root, "config")); errors.Is(err, os.ErrNotExist) {
		cfg := gitconfig.NewConfig()
		cfg.Core.IsBare = true
		if err := s.SetConfig(cfg); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}

	return nil
}

func (s *diskStorer) RawObjectWriter(typ plumbing.ObjectType, sz int64) (io.WriteCloser, error) {
	if err := s.checkContext(); err != nil {
		return nil, err
	}
	if !typ.Valid() {
		return nil, plumbing.ErrInvalidType
	}
	if sz < 0 {
		return nil, fmt.Errorf("negative object size")
	}

	tmpDir := filepath.Join(s.root, "objects", "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("create tmp object dir: %w", err)
	}

	tmpFile, err := os.CreateTemp(tmpDir, "obj-*")
	if err != nil {
		return nil, fmt.Errorf("create temp object: %w", err)
	}

	writer := objfile.NewWriter(tmpFile)
	if err := writer.WriteHeader(typ, sz); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return nil, err
	}

	return &diskObjectWriter{
		storer:   s,
		file:     tmpFile,
		writer:   writer,
		expected: sz,
	}, nil
}

func (s *diskStorer) NewEncodedObject() plumbing.EncodedObject {
	return plumbing.NewMemoryObject(nil)
}

func (s *diskStorer) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	if err := s.checkContext(); err != nil {
		return plumbing.ZeroHash, err
	}

	reader, err := obj.Reader()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	defer reader.Close()

	writer, err := s.RawObjectWriter(obj.Type(), obj.Size())
	if err != nil {
		return plumbing.ZeroHash, err
	}

	if _, err := io.Copy(writer, reader); err != nil {
		_ = writer.Close()
		return plumbing.ZeroHash, err
	}

	if err := writer.Close(); err != nil {
		return plumbing.ZeroHash, err
	}

	hashingWriter, ok := writer.(*diskObjectWriter)
	if !ok {
		return plumbing.ZeroHash, fmt.Errorf("unexpected object writer type")
	}

	return hashingWriter.hash, nil
}

func (s *diskStorer) EncodedObject(typ plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) {
	if err := s.checkContext(); err != nil {
		return nil, err
	}

	file, err := os.Open(s.objectPath(hash))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, plumbing.ErrObjectNotFound
		}
		return nil, fmt.Errorf("open object: %w", err)
	}
	defer file.Close()

	reader, err := objfile.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	actualType, size, err := reader.Header()
	if err != nil {
		return nil, err
	}
	if typ != plumbing.AnyObject && actualType != typ {
		return nil, plumbing.ErrObjectNotFound
	}

	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if err := s.checkContext(); err != nil {
		return nil, err
	}

	obj := plumbing.NewMemoryObject(nil)
	obj.SetType(actualType)
	obj.SetSize(size)
	writer, err := obj.Writer()
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(content); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}

	return obj, nil
}

func (s *diskStorer) IterEncodedObjects(typ plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	if err := s.checkContext(); err != nil {
		return nil, err
	}

	var hashes []plumbing.Hash
	objectsDir := filepath.Join(s.root, "objects")
	err := filepath.WalkDir(objectsDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := s.checkContext(); err != nil {
			return err
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if path == objectsDir {
				return nil
			}
			if base == "tmp" || base == "pack" || base == "info" {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(objectsDir, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 38 {
			return nil
		}

		hash := plumbing.NewHash(parts[0] + parts[1])
		if hash.IsZero() {
			return nil
		}
		hashes = append(hashes, hash)
		return nil
	})
	if err != nil {
		return nil, err
	}

	plumbing.HashesSort(hashes)
	return storer.NewEncodedObjectLookupIter(s, typ, hashes), nil
}

func (s *diskStorer) HasEncodedObject(hash plumbing.Hash) error {
	if err := s.checkContext(); err != nil {
		return err
	}

	_, err := os.Stat(s.objectPath(hash))
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return plumbing.ErrObjectNotFound
	}
	return err
}

func (s *diskStorer) EncodedObjectSize(hash plumbing.Hash) (int64, error) {
	if err := s.checkContext(); err != nil {
		return 0, err
	}

	file, err := os.Open(s.objectPath(hash))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, plumbing.ErrObjectNotFound
		}
		return 0, err
	}
	defer file.Close()

	reader, err := objfile.NewReader(file)
	if err != nil {
		return 0, err
	}
	defer reader.Close()

	_, size, err := reader.Header()
	return size, err
}

func (s *diskStorer) AddAlternate(string) error {
	return s.checkContext()
}

func (s *diskStorer) SetReference(ref *plumbing.Reference) error {
	if err := s.checkContext(); err != nil {
		return err
	}
	done := s.logMetadataLockBoundary("set-reference", ref.Name().String())
	defer func() { done(nil) }()

	data := ref.Strings()[1] + "\n"
	err := s.writeFileAtomic(s.referencePath(ref.Name()), []byte(data), 0o644)
	done(err)
	return err
}

func (s *diskStorer) CheckAndSetReference(newRef, old *plumbing.Reference) error {
	if err := s.checkContext(); err != nil {
		return err
	}
	done := s.logMetadataLockBoundary("check-and-set-reference", newRef.Name().String())
	defer func() { done(nil) }()

	current, err := s.Reference(newRef.Name())
	if old == nil {
		if err == nil {
			err = s.writeReference(newRef)
			done(err)
			return err
		}
		if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			done(err)
			return err
		}
		err = s.writeReference(newRef)
		done(err)
		return err
	}
	if err != nil {
		done(err)
		return err
	}
	if !referencesEqual(current, old) {
		err = storage.ErrReferenceHasChanged
		done(err)
		return err
	}
	err = s.writeReference(newRef)
	done(err)
	return err
}

func (s *diskStorer) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	if err := s.checkContext(); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(s.referencePath(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, plumbing.ErrReferenceNotFound
		}
		return nil, err
	}

	return plumbing.NewReferenceFromStrings(name.String(), strings.TrimSpace(string(data))), nil
}

func (s *diskStorer) IterReferences() (storer.ReferenceIter, error) {
	if err := s.checkContext(); err != nil {
		return nil, err
	}

	var refs []*plumbing.Reference
	if head, err := s.Reference(plumbing.HEAD); err == nil {
		refs = append(refs, head)
	} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, err
	}

	refsDir := filepath.Join(s.root, "refs")
	err := filepath.WalkDir(refsDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if err := s.checkContext(); err != nil {
			return err
		}

		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		ref, err := s.Reference(plumbing.ReferenceName(filepath.ToSlash(rel)))
		if err != nil {
			return err
		}
		refs = append(refs, ref)
		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.SortFunc(refs, func(a, b *plumbing.Reference) int {
		return strings.Compare(a.Name().String(), b.Name().String())
	})

	return storer.NewReferenceSliceIter(refs), nil
}

func (s *diskStorer) RemoveReference(name plumbing.ReferenceName) error {
	if err := s.checkContext(); err != nil {
		return err
	}
	done := s.logMetadataLockBoundary("remove-reference", name.String())
	defer func() { done(nil) }()

	err := os.Remove(s.referencePath(name))
	if errors.Is(err, os.ErrNotExist) {
		err = plumbing.ErrReferenceNotFound
	}
	done(err)
	return err
}

func (s *diskStorer) CountLooseRefs() (int, error) {
	if err := s.checkContext(); err != nil {
		return 0, err
	}

	count := 0
	err := filepath.WalkDir(filepath.Join(s.root, "refs"), func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	return count, err
}

func (s *diskStorer) PackRefs() error {
	return s.checkContext()
}

func (s *diskStorer) SetShallow(hashes []plumbing.Hash) error {
	if err := s.checkContext(); err != nil {
		return err
	}
	done := s.logMetadataLockBoundary("set-shallow", "shallow")
	defer func() { done(nil) }()

	if len(hashes) == 0 {
		err := os.Remove(filepath.Join(s.root, "shallow"))
		if errors.Is(err, os.ErrNotExist) {
			done(nil)
			return nil
		}
		done(err)
		return err
	}

	var buf bytes.Buffer
	for _, hash := range hashes {
		buf.WriteString(hash.String())
		buf.WriteByte('\n')
	}
	err := s.writeFileAtomic(filepath.Join(s.root, "shallow"), buf.Bytes(), 0o644)
	done(err)
	return err
}

func (s *diskStorer) Shallow() ([]plumbing.Hash, error) {
	if err := s.checkContext(); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(filepath.Join(s.root, "shallow"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	lines := strings.Fields(string(data))
	hashes := make([]plumbing.Hash, 0, len(lines))
	for _, line := range lines {
		hash := plumbing.NewHash(line)
		if hash.IsZero() {
			continue
		}
		hashes = append(hashes, hash)
	}
	return hashes, nil
}

func (s *diskStorer) Index() (*formatindex.Index, error) {
	if err := s.checkContext(); err != nil {
		return nil, err
	}

	path := filepath.Join(s.root, "index")
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &formatindex.Index{Version: 2}, nil
		}
		return nil, err
	}
	defer file.Close()

	idx := &formatindex.Index{}
	decoder := formatindex.NewDecoder(file, s.newIndexHash())
	if err := decoder.Decode(idx); err != nil {
		return nil, err
	}
	return idx, nil
}

func (s *diskStorer) SetIndex(idx *formatindex.Index) error {
	if err := s.checkContext(); err != nil {
		return err
	}
	done := s.logMetadataLockBoundary("set-index", "index")
	defer func() { done(nil) }()

	if idx == nil {
		idx = &formatindex.Index{Version: 2}
	}
	if idx.Version == 0 {
		idx.Version = 2
	}

	var buf bytes.Buffer
	encoder := formatindex.NewEncoder(&buf, s.newIndexHash())
	if err := encoder.Encode(idx); err != nil {
		done(err)
		return err
	}
	err := s.writeFileAtomic(filepath.Join(s.root, "index"), buf.Bytes(), 0o644)
	done(err)
	return err
}

func (s *diskStorer) Config() (*gitconfig.Config, error) {
	if err := s.checkContext(); err != nil {
		return nil, err
	}

	file, err := os.Open(filepath.Join(s.root, "config"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg := gitconfig.NewConfig()
			cfg.Core.IsBare = true
			return cfg, nil
		}
		return nil, err
	}
	defer file.Close()

	cfg, err := gitconfig.ReadConfig(file)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func (s *diskStorer) SetConfig(cfg *gitconfig.Config) error {
	if err := s.checkContext(); err != nil {
		return err
	}
	done := s.logMetadataLockBoundary("set-config", "config")
	defer func() { done(nil) }()

	if cfg == nil {
		cfg = gitconfig.NewConfig()
	}
	cfg.Core.IsBare = true

	data, err := cfg.Marshal()
	if err != nil {
		done(err)
		return err
	}
	err = s.writeFileAtomic(filepath.Join(s.root, "config"), data, 0o644)
	done(err)
	return err
}

func (s *diskStorer) writeReference(ref *plumbing.Reference) error {
	data := ref.Strings()[1] + "\n"
	return s.writeFileAtomic(s.referencePath(ref.Name()), []byte(data), 0o644)
}

func (s *diskStorer) logMetadataLockBoundary(operation, target string) func(error) {
	started := time.Now()
	finished := false
	log.Printf("metadata lock start repo_root=%s operation=%s target=%s assumption=immutable-objects", s.root, operation, target)

	return func(err error) {
		if finished {
			return
		}
		finished = true
		duration := time.Since(started).Round(time.Millisecond)
		if err != nil {
			log.Printf("metadata lock end repo_root=%s operation=%s target=%s duration=%s assumption=immutable-objects err=%v", s.root, operation, target, duration, err)
			return
		}
		log.Printf("metadata lock end repo_root=%s operation=%s target=%s duration=%s assumption=immutable-objects", s.root, operation, target, duration)
	}
}

func (s *diskStorer) Module(name string) (storage.Storer, error) {
	if err := s.checkContext(); err != nil {
		return nil, err
	}

	clean, err := normalizeRepoPath(name)
	if err != nil {
		return nil, err
	}
	return newDiskStorer(s.ctx, filepath.Join(s.root, "modules", filepath.FromSlash(clean))), nil
}

func (s *diskStorer) objectPath(hash plumbing.Hash) string {
	hex := hash.String()
	return filepath.Join(s.root, "objects", hex[:2], hex[2:])
}

func (s *diskStorer) referencePath(name plumbing.ReferenceName) string {
	return filepath.Join(s.root, filepath.FromSlash(name.String()))
}

func (s *diskStorer) writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := s.checkContext(); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpName, path)
}

func (s *diskStorer) checkContext() error {
	if s.ctx == nil {
		return nil
	}
	return s.ctx.Err()
}

func (s *diskStorer) objectFormat() formatcfg.ObjectFormat {
	cfg, err := s.Config()
	if err != nil || cfg == nil {
		return formatcfg.SHA1
	}
	if cfg.Extensions.ObjectFormat == formatcfg.UnsetObjectFormat {
		return formatcfg.SHA1
	}
	return cfg.Extensions.ObjectFormat
}

func (s *diskStorer) newIndexHash() plumbinghash.Hash {
	h, err := plumbinghash.FromObjectFormat(s.objectFormat())
	if err != nil {
		fallback, _ := plumbinghash.FromObjectFormat(formatcfg.SHA1)
		return fallback
	}
	return h
}

type diskObjectWriter struct {
	storer   *diskStorer
	file     *os.File
	writer   *objfile.Writer
	expected int64
	written  int64
	hash     plumbing.Hash
	closed   bool
}

func (w *diskObjectWriter) Write(p []byte) (int, error) {
	if err := w.storer.checkContext(); err != nil {
		return 0, err
	}
	n, err := w.writer.Write(p)
	w.written += int64(n)
	return n, err
}

func (w *diskObjectWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	defer os.Remove(w.file.Name())

	if err := w.storer.checkContext(); err != nil {
		_ = w.file.Close()
		return err
	}
	if w.written != w.expected {
		_ = w.writer.Close()
		_ = w.file.Close()
		return fmt.Errorf("object size mismatch: wrote %d bytes, expected %d", w.written, w.expected)
	}
	if err := w.writer.Close(); err != nil {
		_ = w.file.Close()
		return err
	}
	if err := w.file.Close(); err != nil {
		return err
	}

	w.hash = w.writer.Hash()
	finalPath := w.storer.objectPath(w.hash)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return err
	}

	if _, err := os.Stat(finalPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return os.Rename(w.file.Name(), finalPath)
}

func referencesEqual(a, b *plumbing.Reference) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Name() != b.Name() || a.Type() != b.Type() {
		return false
	}
	switch a.Type() {
	case plumbing.HashReference:
		return a.Hash() == b.Hash()
	case plumbing.SymbolicReference:
		return a.Target() == b.Target()
	default:
		return false
	}
}

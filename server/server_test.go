package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"golang.org/x/crypto/ssh"
)

func TestHTTPAuthWithGitCLI(t *testing.T) {
	t.Parallel()

	srv, repoDir := startTestServer(t)
	worktree := createWorktree(t)

	httpRemote := srv.HTTPBaseURL() + "/test.git"
	pushURL := strings.Replace(httpRemote, "http://", "http://username:password@", 1)

	runGit(t, worktree, nil, "push", pushURL, "HEAD:refs/heads/main")
	runGit(t, repoDir, nil, "--git-dir", repoDir, "rev-parse", "refs/heads/main")

	output, err := runGitExpectError(t, worktree, nil, "ls-remote", strings.Replace(httpRemote, "http://", "http://bad:creds@", 1))
	if err == nil {
		t.Fatalf("expected bad HTTP credentials to fail")
	}
	assertContainsAny(t, output, "Authentication failed", "authentication failed", "HTTP Basic: Access denied", "401")
}

func TestSSHAuthWithGitCLI(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat(filepath.Join(mustHomeDir(t), ".ssh", "id_rsa")); err != nil {
		t.Skipf("skipping: ~/.ssh/id_rsa is required: %v", err)
	}

	srv, repoDir := startTestServer(t)
	seedRepo(t, repoDir)

	successEnv := []string{
		"GIT_SSH_COMMAND=" + sshCommand(t, filepath.Join(mustHomeDir(t), ".ssh", "id_rsa"), srv.sshListener.Addr().String()),
	}
	output := runGit(t, t.TempDir(), successEnv, "ls-remote", srv.SSHRemote("test.git"))
	if !strings.Contains(output, "refs/heads/main") {
		t.Fatalf("expected ls-remote output to contain refs/heads/main, got: %s", output)
	}

	wrongKey := writeWrongPrivateKey(t)
	output, err := runGitExpectError(t, t.TempDir(), []string{
		"GIT_SSH_COMMAND=" + sshCommand(t, wrongKey, srv.sshListener.Addr().String()),
	}, "ls-remote", srv.SSHRemote("test.git"))
	if err == nil {
		t.Fatalf("expected wrong SSH key to fail")
	}
	assertContainsAny(t, output, "Permission denied", "publickey", "rejected", "Could not read from remote repository")
}

func TestInitBareRepoRejectsPathTraversal(t *testing.T) {
	t.Parallel()

	err := InitBareRepo(t.TempDir(), "../evil.git")
	if !errors.Is(err, ErrInvalidRepoPath) {
		t.Fatalf("expected ErrInvalidRepoPath, got %v", err)
	}
}

func TestOperationContextSetsThirtySecondDeadline(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	before := time.Now()
	ctx, cancel := srv.operationContext(context.Background(), nil)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("expected deadline to be set")
	}

	diff := deadline.Sub(before)
	if diff < 29*time.Second || diff > 31*time.Second {
		t.Fatalf("expected ~30s deadline, got %v", diff)
	}
}

func TestPreviewReceivePackRequestReplaysOriginalStream(t *testing.T) {
	t.Parallel()

	updateReq := packp.NewUpdateRequests()
	updateReq.Commands = []*packp.Command{
		{
			Name: plumbing.NewBranchReferenceName("feature"),
			Old:  plumbing.NewHash(strings.Repeat("1", 40)),
			New:  plumbing.NewHash(strings.Repeat("2", 40)),
		},
		{
			Name: plumbing.NewBranchReferenceName("main"),
			Old:  plumbing.ZeroHash,
			New:  plumbing.NewHash(strings.Repeat("3", 40)),
		},
	}

	var payload bytes.Buffer
	if err := updateReq.Encode(&payload); err != nil {
		t.Fatalf("encode update request: %v", err)
	}

	original := append([]byte(nil), payload.Bytes()...)
	original = append(original, []byte("PACKtoydata")...)

	replayed, details := previewReceivePackRequest(bytes.NewReader(original))
	if details.parseErr != nil {
		t.Fatalf("preview receive-pack request: %v", details.parseErr)
	}

	if got, want := strings.Join(details.commands, ","), "create:refs/heads/main,update:refs/heads/feature"; got != want {
		t.Fatalf("unexpected command summary: got %q want %q", got, want)
	}

	got, err := io.ReadAll(replayed)
	if err != nil {
		t.Fatalf("read replayed stream: %v", err)
	}

	if !bytes.Equal(got, original) {
		t.Fatalf("replayed stream did not match original")
	}
}

func TestLFSTransferOverHTTP(t *testing.T) {
	t.Parallel()

	srv, _ := startTestServer(t)
	worktree, original := createLFSWorktree(t)

	httpRemote := srv.HTTPBaseURL() + "/test.git"
	pushURL := strings.Replace(httpRemote, "http://", "http://username:password@", 1)

	runGit(t, worktree, nil, "remote", "add", "origin", pushURL)
	runGit(t, worktree, nil, "-c", "lfs.locksverify=false", "push", "origin", "HEAD:refs/heads/main")

	cloneDir := t.TempDir()
	runGit(t, t.TempDir(), nil, "clone", strings.Replace(httpRemote, "http://", "http://username:password@", 1), cloneDir)
	runGit(t, cloneDir, nil, "lfs", "install", "--local")
	runGit(t, cloneDir, nil, "lfs", "pull")

	got, err := os.ReadFile(filepath.Join(cloneDir, "large.bin"))
	if err != nil {
		t.Fatalf("read cloned LFS file: %v", err)
	}
	if string(got) != original {
		t.Fatalf("expected LFS content to round-trip over HTTP")
	}
}

func TestLFSTransferOverSSH(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat(filepath.Join(mustHomeDir(t), ".ssh", "id_rsa")); err != nil {
		t.Skipf("skipping: ~/.ssh/id_rsa is required: %v", err)
	}

	srv, _ := startTestServer(t)
	worktree, original := createLFSWorktree(t)

	sshEnv := []string{
		"GIT_SSH_COMMAND=" + sshCommand(t, filepath.Join(mustHomeDir(t), ".ssh", "id_rsa"), srv.sshListener.Addr().String()),
	}
	runGit(t, worktree, sshEnv, "remote", "add", "origin", srv.SSHRemote("test.git"))
	runGit(t, worktree, sshEnv, "-c", "lfs.locksverify=false", "push", "origin", "HEAD:refs/heads/main")

	cloneDir := filepath.Join(t.TempDir(), "clone")
	runGit(t, t.TempDir(), sshEnv, "clone", srv.SSHRemote("test.git"), cloneDir)
	runGit(t, cloneDir, sshEnv, "lfs", "install", "--local")
	runGit(t, cloneDir, sshEnv, "lfs", "pull")

	got, err := os.ReadFile(filepath.Join(cloneDir, "large.bin"))
	if err != nil {
		t.Fatalf("read cloned LFS file: %v", err)
	}
	if string(got) != original {
		t.Fatalf("expected LFS content to round-trip over SSH")
	}
}

func startTestServer(t *testing.T) (*Server, string) {
	t.Helper()

	dataDir := t.TempDir()
	if err := InitBareRepo(dataDir, "test.git"); err != nil {
		t.Fatalf("init bare repo: %v", err)
	}

	srv, err := NewServer(Config{
		DataDir: dataDir,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Fatalf("close server: %v", err)
		}
	})

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("start server: %v", err)
	}

	return srv, filepath.Join(dataDir, "test.git")
}

func createWorktree(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	runGit(t, dir, nil, "init")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write worktree file: %v", err)
	}
	runGit(t, dir, nil, "add", "README.md")
	runGit(t, dir, nil, "-c", "user.name=Codex", "-c", "user.email=codex@example.com", "commit", "-m", "initial commit")
	return dir
}

func createLFSWorktree(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()
	runGit(t, dir, nil, "init")
	runGit(t, dir, nil, "lfs", "install", "--local")
	runGit(t, dir, nil, "lfs", "track", "*.bin")

	content := strings.Repeat("toy-lfs-payload-", 1024)
	if err := os.WriteFile(filepath.Join(dir, "large.bin"), []byte(content), 0o644); err != nil {
		t.Fatalf("write LFS file: %v", err)
	}
	runGit(t, dir, nil, "add", ".gitattributes", "large.bin")
	runGit(t, dir, nil, "-c", "user.name=Codex", "-c", "user.email=codex@example.com", "commit", "-m", "add lfs blob")
	return dir, content
}

func seedRepo(t *testing.T, repoDir string) {
	t.Helper()

	worktree := createWorktree(t)
	runGit(t, worktree, nil, "push", repoDir, "HEAD:refs/heads/main")
}

func sshCommand(t *testing.T, privateKeyPath, addr string) string {
	t.Helper()

	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host/port: %v", err)
	}
	return fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o PreferredAuthentications=publickey -p %s", privateKeyPath, port)
}

func runGit(t *testing.T, dir string, extraEnv []string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PROTOCOL=version=0",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, output)
	}
	return string(output)
}

func runGitExpectError(t *testing.T, dir string, extraEnv []string, args ...string) (string, error) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PROTOCOL=version=0",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return string(output), nil
	}
	return string(output), err
}

func writeWrongPrivateKey(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	privateKeyPath := filepath.Join(t.TempDir(), "id_rsa")
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(privateKeyPath, privateKeyPEM, 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}

	publicKey, err := ssh.NewPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("create public key: %v", err)
	}
	if err := os.WriteFile(privateKeyPath+".pub", ssh.MarshalAuthorizedKey(publicKey), 0o644); err != nil {
		t.Fatalf("write public key: %v", err)
	}

	return privateKeyPath
}

func mustHomeDir(t *testing.T) string {
	t.Helper()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home dir: %v", err)
	}
	return home
}

func assertContainsAny(t *testing.T, output string, needles ...string) {
	t.Helper()

	for _, needle := range needles {
		if strings.Contains(output, needle) {
			return
		}
	}

	t.Fatalf("expected output to contain one of %q, got: %s", needles, output)
}

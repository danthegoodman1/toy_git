package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"git-demo/server"
)

func main() {
	var (
		dataDir      = flag.String("data-dir", "./data", "directory used to store repositories")
		httpAddr     = flag.String("http-addr", "127.0.0.1:8080", "HTTP listen address")
		sshAddr      = flag.String("ssh-addr", "127.0.0.1:2222", "SSH listen address")
		repos        = flag.String("repos", "test.git", "comma-separated repo names to initialize")
		allowedKey   = flag.String("authorized-key", "~/.ssh/id_rsa.pub", "path to the single allowed SSH public key")
		httpUsername = flag.String("http-username", "username", "HTTP basic auth username")
		httpPassword = flag.String("http-password", "password", "HTTP basic auth password")
	)
	flag.Parse()

	repoList := splitRepos(*repos)
	if len(repoList) == 0 {
		fatalf("no repos specified")
	}

	for _, repo := range repoList {
		if err := server.InitBareRepo(*dataDir, repo); err != nil {
			fatalf("init repo %s: %v", repo, err)
		}
	}

	srv, err := server.NewServer(server.Config{
		DataDir:              *dataDir,
		HTTPAddr:             *httpAddr,
		SSHAddr:              *sshAddr,
		AllowedPublicKeyPath: *allowedKey,
		HTTPUsername:         *httpUsername,
		HTTPPassword:         *httpPassword,
	})
	if err != nil {
		fatalf("create server: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Start(ctx); err != nil {
		fatalf("start server: %v", err)
	}
	defer func() {
		if err := srv.Close(); err != nil {
			fatalf("close server: %v", err)
		}
	}()

	fmt.Printf("git-demo server running\n")
	fmt.Printf("HTTP base URL: %s\n", srv.HTTPBaseURL())
	fmt.Printf("SSH key allowed: %s\n", *allowedKey)
	fmt.Printf("HTTP credentials: %s / %s\n", *httpUsername, *httpPassword)
	fmt.Printf("\n")

	for _, repo := range repoList {
		httpRemote := strings.TrimSuffix(srv.HTTPBaseURL(), "/") + "/" + strings.TrimPrefix(repo, "/")
		fmt.Printf("Repo: %s\n", repo)
		fmt.Printf("  HTTP remote: http://%s:%s@%s\n", *httpUsername, *httpPassword, strings.TrimPrefix(httpRemote, "http://"))
		fmt.Printf("  SSH remote:  %s\n", srv.SSHRemote(repo))
		fmt.Printf("  SSH push:    GIT_SSH_COMMAND='ssh -i ~/.ssh/id_rsa -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p %s' git push <remote> HEAD:refs/heads/main\n", portFromRemote(srv.SSHRemote(repo)))
		fmt.Printf("\n")
	}

	fmt.Printf("Press Ctrl+C to stop.\n")
	<-ctx.Done()
	fmt.Printf("\nshutting down...\n")
}

func splitRepos(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func portFromRemote(remote string) string {
	lastColon := strings.LastIndex(remote, ":")
	if lastColon == -1 || lastColon+1 >= len(remote) {
		return ""
	}
	return remote[lastColon+1:]
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

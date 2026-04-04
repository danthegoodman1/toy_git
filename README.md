# git-demo

A toy Git server in Go using `go-git/v6`.

It serves:
- Git smart HTTP
- Git over SSH
- Git LFS over HTTP
- Git LFS for SSH remotes via `git-lfs-authenticate` -> HTTP LFS transfer

Storage is local and disk-backed under `./data`.

## Auth

- SSH: only the public key at `~/.ssh/id_rsa.pub` is accepted by default
- HTTP: basic auth with `username` / `password`

## Run

```bash
go run ./cmd/git-demo
```

Default listeners:
- HTTP: `127.0.0.1:8080`
- SSH: `127.0.0.1:2222`
- Repo: `test.git`

You can change them with flags:

```bash
go run ./cmd/git-demo \
  -data-dir ./data \
  -http-addr 127.0.0.1:8080 \
  -ssh-addr 127.0.0.1:2222 \
  -repos test.git
```

## Push

HTTP:

```bash
git remote add demo http://username:password@127.0.0.1:8080/test.git
git push demo HEAD:refs/heads/main
```

SSH:

```bash
git remote add demo ssh://git@127.0.0.1:2222/test.git
GIT_SSH_COMMAND='ssh -i ~/.ssh/id_rsa -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p 2222' \
git push demo HEAD:refs/heads/main
```

## Clone

HTTP:

```bash
git clone http://username:password@127.0.0.1:8080/test.git
```

SSH:

```bash
GIT_SSH_COMMAND='ssh -i ~/.ssh/id_rsa -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p 2222' \
git clone ssh://git@127.0.0.1:2222/test.git
```

## LFS

If your repo uses Git LFS, the same remotes work.

Example:

```bash
git lfs install
git lfs track "*.bin"
git add .gitattributes large.bin
git commit -m "add lfs file"
git push demo HEAD:refs/heads/main
git lfs pull
```

Note: for SSH remotes, normal Git traffic goes over SSH, but LFS object transfer is redirected to the server's HTTP LFS API after SSH authentication.

## Test

```bash
go test ./...
```
# toy_git

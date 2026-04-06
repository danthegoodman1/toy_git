package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"golang.org/x/crypto/ssh"
)

const (
	defaultDataDir       = "./data"
	defaultHTTPAddr      = "127.0.0.1:0"
	defaultSSHAddr       = "127.0.0.1:0"
	defaultHTTPUsername  = "username"
	defaultHTTPPassword  = "password"
	defaultAuthorizedKey = "~/.ssh/id_rsa.pub"
	maxOperationTime     = 30 * time.Second
)

var gitExecPattern = regexp.MustCompile(`^(git-upload-pack|git-receive-pack)\s+'?([^']+)'?$`)

type Config struct {
	DataDir              string
	HTTPAddr             string
	SSHAddr              string
	AllowedPublicKeyPath string
	HTTPUsername         string
	HTTPPassword         string
}

type Server struct {
	cfg             Config
	backend         *diskBackend
	allowedKeyBytes []byte

	httpServer   *http.Server
	httpListener net.Listener

	sshConfig   *ssh.ServerConfig
	sshListener net.Listener

	sshConnMu sync.Mutex
	sshConns  map[net.Conn]struct{}

	closeOnce sync.Once
	wg        sync.WaitGroup
}

func NewServer(cfg Config) (*Server, error) {
	cfg, err := applyDefaults(cfg)
	if err != nil {
		return nil, err
	}

	backend, err := newDiskBackend(cfg.DataDir)
	if err != nil {
		return nil, err
	}

	authorizedKey, err := loadAuthorizedKey(cfg.AllowedPublicKeyPath)
	if err != nil {
		return nil, err
	}

	hostSigner, err := generateHostSigner()
	if err != nil {
		return nil, err
	}

	server := &Server{
		cfg:             cfg,
		backend:         backend,
		allowedKeyBytes: authorizedKey.Marshal(),
		sshConns:        make(map[net.Conn]struct{}),
	}

	server.httpServer = &http.Server{
		Handler:           http.HandlerFunc(server.handleHTTP),
		ReadHeaderTimeout: maxOperationTime,
		ReadTimeout:       maxOperationTime,
		WriteTimeout:      maxOperationTime,
		IdleTimeout:       maxOperationTime,
	}

	server.sshConfig = &ssh.ServerConfig{
		PublicKeyCallback: server.authorizePublicKey,
	}
	server.sshConfig.AddHostKey(hostSigner)

	return server, nil
}

func (s *Server) Start(ctx context.Context) error {
	httpListener, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen http: %w", err)
	}

	sshListener, err := net.Listen("tcp", s.cfg.SSHAddr)
	if err != nil {
		_ = httpListener.Close()
		return fmt.Errorf("listen ssh: %w", err)
	}

	s.httpListener = httpListener
	s.sshListener = sshListener

	if ctx != nil {
		go func() {
			<-ctx.Done()
			_ = s.Close()
		}()
	}

	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		_ = s.httpServer.Serve(httpListener)
	}()

	go func() {
		defer s.wg.Done()
		s.serveSSH(sshListener)
	}()

	return nil
}

func (s *Server) Close() error {
	var joined error

	s.closeOnce.Do(func() {
		if s.httpServer != nil {
			joined = errors.Join(joined, s.httpServer.Close())
		}
		if s.sshListener != nil {
			joined = errors.Join(joined, s.sshListener.Close())
		}
		joined = errors.Join(joined, s.closeSSHConnections())
		s.wg.Wait()
	})

	return joined
}

func (s *Server) HTTPBaseURL() string {
	if s.httpListener == nil {
		return ""
	}
	return "http://" + s.httpListener.Addr().String()
}

func (s *Server) SSHRemote(repo string) string {
	if s.sshListener == nil {
		return ""
	}
	normalized, err := normalizeRepoPath(repo)
	if err != nil {
		return ""
	}
	return "ssh://git@" + s.sshListener.Addr().String() + "/" + normalized
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.operationContext(r.Context(), nil)
	defer cancel()

	if !s.authorizeHTTP(w, r) && !s.authorizeLFSHeader(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if s.maybeHandleLFSHTTP(ctx, w, r) {
		return
	}

	repo, service, advertiseRefs, err := parseGitHTTPRequest(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	st, err := s.backend.Open(ctx, repo)
	if err != nil {
		s.writeGitHTTPError(w, err)
		return
	}
	// Open is per-RPC. A snapshot-capable backend would bind the request's
	// stable read view or write transaction state here.

	w.Header().Set("Cache-Control", "no-cache")

	if advertiseRefs {
		w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service.String()))
		// info/refs is read-only. If refs can change concurrently, the read
		// snapshot boundary should cover this whole advertise-refs RPC.
		if err := transport.AdvertiseReferences(ctx, st, w, service, true); err != nil {
			s.writeGitHTTPError(w, err)
		}
		return
	}

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-result", service.String()))
	writeCloser := responseWriteCloser{Writer: w}
	reader := io.ReadCloser(r.Body)

	var lockLog *receivePackLog
	if service == transport.ReceivePackService {
		reader, lockLog = previewReceivePackRequest(r.Body)
		s.logReceivePackBoundaryStart("http", repo, lockLog)
		defer s.logReceivePackBoundaryEnd("http", repo, lockLog)
	}

	switch service {
	case transport.UploadPackService:
		// upload-pack is read-only, but it needs a stable repo view for the
		// whole fetch/clone RPC. A read snapshot or shared lock would start
		// before this call and end when it returns.
		err = transport.UploadPack(ctx, st, reader, writeCloser, &transport.UploadPackOptions{
			StatelessRPC: true,
		})
	case transport.ReceivePackService:
		// receive-pack is the write boundary. A future per-repo/per-ref lock or
		// ref transaction should begin before this call and be released only
		// after the full push RPC completes.
		//
		// If we move past a repo-wide lock, the refs named in this push still
		// need to be locked as one group: acquire the full sorted ref set before
		// mutating anything, then release the whole group after the RPC finishes.
		err = transport.ReceivePack(ctx, st, reader, writeCloser, &transport.ReceivePackOptions{
			StatelessRPC: true,
		})
	default:
		http.NotFound(w, r)
		return
	}

	if err != nil {
		s.writeGitHTTPError(w, err)
	}
}

func (s *Server) authorizeHTTP(w http.ResponseWriter, r *http.Request) bool {
	username, password, ok := r.BasicAuth()
	if ok &&
		subtle.ConstantTimeCompare([]byte(username), []byte(s.cfg.HTTPUsername)) == 1 &&
		subtle.ConstantTimeCompare([]byte(password), []byte(s.cfg.HTTPPassword)) == 1 {
		return true
	}
	return false
}

func (s *Server) writeGitHTTPError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, transport.ErrRepositoryNotFound):
		http.Error(w, transport.ErrRepositoryNotFound.Error(), http.StatusNotFound)
	case errors.Is(err, ErrInvalidRepoPath):
		http.Error(w, ErrInvalidRepoPath.Error(), http.StatusBadRequest)
	case errors.Is(err, context.DeadlineExceeded):
		http.Error(w, "operation timed out", http.StatusGatewayTimeout)
	case errors.Is(err, context.Canceled):
		http.Error(w, "operation canceled", http.StatusRequestTimeout)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func parseGitHTTPRequest(r *http.Request) (repo string, service transport.Service, advertiseRefs bool, err error) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs"):
		service = transport.Service(r.URL.Query().Get("service"))
		repo = strings.TrimSuffix(r.URL.Path, "/info/refs")
		advertiseRefs = true
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
		service = transport.UploadPackService
		repo = strings.TrimSuffix(r.URL.Path, "/git-upload-pack")
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
		service = transport.ReceivePackService
		repo = strings.TrimSuffix(r.URL.Path, "/git-receive-pack")
	default:
		return "", "", false, fmt.Errorf("unsupported route")
	}

	switch service {
	case transport.UploadPackService, transport.ReceivePackService:
	default:
		return "", "", false, fmt.Errorf("unsupported service")
	}

	return repo, service, advertiseRefs, nil
}

func (s *Server) authorizePublicKey(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	if subtle.ConstantTimeCompare(key.Marshal(), s.allowedKeyBytes) != 1 {
		return nil, fmt.Errorf("ssh public key rejected")
	}

	return &ssh.Permissions{}, nil
}

func (s *Server) serveSSH(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}

		s.trackSSHConn(conn)

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrackSSHConn(conn)
			s.handleSSHConn(conn)
		}()
	}
}

func (s *Server) handleSSHConn(conn net.Conn) {
	defer conn.Close()

	serverConn, chans, reqs, err := ssh.NewServerConn(conn, s.sshConfig)
	if err != nil {
		return
	}
	defer serverConn.Close()

	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}

		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleSSHSession(channel, requests)
		}()
	}
}

func (s *Server) handleSSHSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()

	for req := range requests {
		switch req.Type {
		case "exec":
			var payload struct {
				Command string
			}
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
				_ = req.Reply(false, nil)
				return
			}

			_ = req.Reply(true, nil)
			status := s.runSSHCommand(payload.Command, channel, channel.Stderr())
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct {
				Status uint32
			}{Status: status}))
			return
		default:
			_ = req.Reply(false, nil)
		}
	}
}

func (s *Server) runSSHCommand(command string, channel ssh.Channel, stderr io.Writer) uint32 {
	if handled, status := s.tryHandleLFSSSHAuth(command, channel, stderr); handled {
		return status
	}

	service, repo, err := parseGitExecCommand(command)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err.Error())
		return 127
	}

	ctx, cancel := s.operationContext(context.Background(), channel)
	defer cancel()

	st, err := s.backend.Open(ctx, repo)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err.Error())
		return 1
	}
	// Open is per-RPC. A snapshot-capable backend would bind the request's
	// stable read view or write transaction state here.

	reader := io.NopCloser(channel)
	writer := channelWriteCloser{Channel: channel}
	var readStream io.ReadCloser = reader

	var lockLog *receivePackLog
	var preview *receivePackPreviewReader
	if service == transport.ReceivePackService {
		preview = newReceivePackPreviewReader(reader)
		readStream = preview
		lockLog = &receivePackLog{started: time.Now()}
		s.logReceivePackBoundaryStart("ssh", repo, lockLog)
		defer func() {
			recorded := preview.details()
			recorded.started = lockLog.started
			s.logReceivePackBoundaryEnd("ssh", repo, recorded)
		}()
	}

	switch service {
	case transport.UploadPackService:
		// upload-pack is read-only, but it needs a stable repo view for the
		// whole fetch/clone RPC. A read snapshot or shared lock would start
		// before this call and end when it returns.
		err = transport.UploadPack(ctx, st, readStream, writer, nil)
	case transport.ReceivePackService:
		// receive-pack is the write boundary. A future per-repo/per-ref lock or
		// ref transaction should begin before this call and be released only
		// after the full push RPC completes.
		//
		// If we move past a repo-wide lock, the refs named in this push still
		// need to be locked as one group: acquire the full sorted ref set before
		// mutating anything, then release the whole group after the RPC finishes.
		err = transport.ReceivePack(ctx, st, readStream, writer, nil)
	default:
		err = fmt.Errorf("unsupported service %q", service)
	}

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			_, _ = fmt.Fprintln(stderr, "operation timed out")
			return 124
		}
		if errors.Is(err, context.Canceled) {
			_, _ = fmt.Fprintln(stderr, "operation canceled")
			return 124
		}
		_, _ = fmt.Fprintln(stderr, err.Error())
		return 1
	}

	return 0
}

func parseGitExecCommand(command string) (transport.Service, string, error) {
	matches := gitExecPattern.FindStringSubmatch(strings.TrimSpace(command))
	if matches == nil {
		return "", "", fmt.Errorf("unsupported exec command")
	}

	service := transport.Service(matches[1])
	switch service {
	case transport.UploadPackService, transport.ReceivePackService:
	default:
		return "", "", fmt.Errorf("unsupported exec command")
	}

	return service, matches[2], nil
}

func applyDefaults(cfg Config) (Config, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = defaultDataDir
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = defaultHTTPAddr
	}
	if cfg.SSHAddr == "" {
		cfg.SSHAddr = defaultSSHAddr
	}
	if cfg.AllowedPublicKeyPath == "" {
		cfg.AllowedPublicKeyPath = defaultAuthorizedKey
	}
	if cfg.HTTPUsername == "" {
		cfg.HTTPUsername = defaultHTTPUsername
	}
	if cfg.HTTPPassword == "" {
		cfg.HTTPPassword = defaultHTTPPassword
	}

	var err error
	cfg.DataDir, err = filepath.Abs(cfg.DataDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve data dir: %w", err)
	}

	cfg.AllowedPublicKeyPath, err = expandHome(cfg.AllowedPublicKeyPath)
	if err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func expandHome(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return filepath.Abs(path)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}

	if path == "~" {
		return home, nil
	}

	if !strings.HasPrefix(path, "~/") {
		return "", fmt.Errorf("unsupported home path %q", path)
	}

	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

func loadAuthorizedKey(path string) (ssh.PublicKey, error) {
	keyBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read authorized key: %w", err)
	}

	key, _, _, _, err := ssh.ParseAuthorizedKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse authorized key: %w", err)
	}

	return key, nil
}

func generateHostSigner() (ssh.Signer, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate host key: %w", err)
	}

	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("create host signer: %w", err)
	}

	return signer, nil
}

func (s *Server) operationContext(parent context.Context, closer io.Closer) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, maxOperationTime)
	if closer == nil {
		return ctx, cancel
	}

	go func() {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			_ = closer.Close()
		}
	}()

	return ctx, cancel
}

func (s *Server) trackSSHConn(conn net.Conn) {
	s.sshConnMu.Lock()
	defer s.sshConnMu.Unlock()
	s.sshConns[conn] = struct{}{}
}

func (s *Server) untrackSSHConn(conn net.Conn) {
	s.sshConnMu.Lock()
	defer s.sshConnMu.Unlock()
	delete(s.sshConns, conn)
}

func (s *Server) closeSSHConnections() error {
	s.sshConnMu.Lock()
	defer s.sshConnMu.Unlock()

	var joined error
	for conn := range s.sshConns {
		joined = errors.Join(joined, conn.Close())
	}
	return joined
}

type responseWriteCloser struct {
	Writer io.Writer
}

func (w responseWriteCloser) Write(p []byte) (int, error) {
	return w.Writer.Write(p)
}

func (responseWriteCloser) Close() error {
	return nil
}

type channelWriteCloser struct {
	ssh.Channel
}

func (w channelWriteCloser) Close() error {
	return w.Channel.CloseWrite()
}

type receivePackLog struct {
	commands []string
	parseErr error
	started  time.Time
}

type receivePackPreviewReader struct {
	source  io.ReadCloser
	preview receivePackPreview
}

type receivePackPreview struct {
	prefix   bytes.Buffer
	pending  []byte
	parseErr error
	complete bool
}

func previewReceivePackRequest(r io.Reader) (io.ReadCloser, *receivePackLog) {
	buffered := bufio.NewReader(r)
	var prefix bytes.Buffer

	request := packp.NewUpdateRequests()
	parseErr := request.Decode(io.TeeReader(buffered, &prefix))

	return io.NopCloser(io.MultiReader(bytes.NewReader(prefix.Bytes()), buffered)), &receivePackLog{
		commands: summarizeReceivePackCommands(request.Commands),
		parseErr: parseErr,
		started:  time.Now(),
	}
}

func newReceivePackPreviewReader(source io.ReadCloser) *receivePackPreviewReader {
	return &receivePackPreviewReader{source: source}
}

func (r *receivePackPreviewReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if n > 0 {
		r.preview.consume(p[:n])
	}
	return n, err
}

func (r *receivePackPreviewReader) Close() error {
	return r.source.Close()
}

func (r *receivePackPreviewReader) details() *receivePackLog {
	return r.preview.details()
}

func (p *receivePackPreview) consume(chunk []byte) {
	if p.complete || len(chunk) == 0 {
		return
	}

	p.pending = append(p.pending, chunk...)
	for {
		if len(p.pending) < 4 {
			return
		}

		pktLen, err := strconv.ParseUint(string(p.pending[:4]), 16, 16)
		if err != nil {
			p.parseErr = err
			p.complete = true
			p.pending = nil
			return
		}

		if pktLen == 0 {
			_, _ = p.prefix.Write(p.pending[:4])
			p.complete = true
			p.pending = nil
			return
		}
		if pktLen < 4 {
			p.parseErr = fmt.Errorf("invalid pkt-line length %d", pktLen)
			p.complete = true
			p.pending = nil
			return
		}
		if len(p.pending) < int(pktLen) {
			return
		}

		_, _ = p.prefix.Write(p.pending[:pktLen])
		p.pending = p.pending[pktLen:]
	}
}

func (p *receivePackPreview) details() *receivePackLog {
	request := packp.NewUpdateRequests()
	parseErr := p.parseErr
	if parseErr == nil {
		parseErr = request.Decode(bytes.NewReader(p.prefix.Bytes()))
	}

	return &receivePackLog{
		commands: summarizeReceivePackCommands(request.Commands),
		parseErr: parseErr,
	}
}

func summarizeReceivePackCommands(commands []*packp.Command) []string {
	if len(commands) == 0 {
		return nil
	}

	summary := make([]string, 0, len(commands))
	for _, command := range commands {
		summary = append(summary, fmt.Sprintf("%s:%s", command.Action(), command.Name))
	}
	sort.Strings(summary)
	return summary
}

func (s *Server) logReceivePackBoundaryStart(protocol, repo string, details *receivePackLog) {
	if details == nil {
		return
	}

	if len(details.commands) == 0 && details.parseErr == nil {
		log.Printf("receive-pack lock start protocol=%s repo=%s updates=pending", protocol, repo)
		return
	}

	if details.parseErr != nil {
		log.Printf("receive-pack lock start protocol=%s repo=%s updates=%v parse_error=%v", protocol, repo, details.commands, details.parseErr)
		return
	}

	log.Printf("receive-pack lock start protocol=%s repo=%s updates=%v", protocol, repo, details.commands)
}

func (s *Server) logReceivePackBoundaryEnd(protocol, repo string, details *receivePackLog) {
	if details == nil {
		return
	}

	duration := time.Since(details.started).Round(time.Millisecond)
	if details.parseErr != nil {
		log.Printf("receive-pack lock end protocol=%s repo=%s duration=%s updates=%v parse_error=%v", protocol, repo, duration, details.commands, details.parseErr)
		return
	}

	log.Printf("receive-pack lock end protocol=%s repo=%s duration=%s updates=%v", protocol, repo, duration, details.commands)
}

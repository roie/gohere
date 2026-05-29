package staticserver

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	port   int
	server *http.Server
	ln     net.Listener
}

type Config struct {
	Dir          string
	Port         int
	Host         string
	Live         bool
	PollInterval time.Duration

	broker *liveBroker
}

func IsStaticProject(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "index.html"))
	return err == nil && !info.IsDir()
}

func Handler(root string) http.Handler {
	return HandlerWithConfig(Config{Dir: root})
}

func HandlerWithConfig(cfg Config) http.Handler {
	root := cfg.Dir
	live := cfg.Live
	broker := cfg.broker
	if live && broker == nil {
		broker = newLiveBroker()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if live && r.URL.Path == liveEndpoint {
			broker.ServeHTTP(w, r)
			return
		}
		localPath, ok := localStaticPath(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		fullPath := filepath.Join(root, localPath)
		info, err := os.Stat(fullPath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if info.IsDir() {
			fullPath = filepath.Join(fullPath, "index.html")
			info, err = os.Stat(fullPath)
			if err != nil || info.IsDir() {
				http.NotFound(w, r)
				return
			}
		}
		if live && isHTMLFile(fullPath) {
			serveLiveHTML(w, r, fullPath, info)
			return
		}
		http.ServeFile(w, r, fullPath)
	})
}

const (
	liveEndpoint     = "/__gohere/live"
	liveClientScript = `<script>(()=>{const source=new EventSource("/__gohere/live");source.onmessage=()=>location.reload();})();</script>`
)

func localStaticPath(urlPath string) (string, bool) {
	if strings.Contains(urlPath, "\\") {
		return "", false
	}
	cleanPath := path.Clean("/" + urlPath)
	if cleanPath == "/" {
		return "index.html", true
	}
	cleanPath = strings.TrimPrefix(cleanPath, "/")
	if cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
		return "", false
	}
	return filepath.FromSlash(cleanPath), true
}

func Start(ctx context.Context, dir string, port int) (*Server, error) {
	return StartWithHost(ctx, dir, port, "127.0.0.1")
}

func StartWithHost(ctx context.Context, dir string, port int, host string) (*Server, error) {
	return StartWithConfig(ctx, Config{Dir: dir, Port: port, Host: host})
}

func StartWithConfig(ctx context.Context, cfg Config) (*Server, error) {
	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	addr := host + ":"
	if cfg.Port != 0 {
		addr += strconv.Itoa(cfg.Port)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	listenPort, err := listenerPort(ln)
	if err != nil {
		ln.Close()
		return nil, err
	}

	if cfg.Live {
		cfg.broker = newLiveBroker()
		initial, err := snapshotStaticRoot(cfg.Dir)
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("snapshot static root: %w", err)
		}
		go watchStaticRoot(ctx, cfg.Dir, cfg.PollInterval, initial, cfg.broker)
	}
	server := &http.Server{Handler: HandlerWithConfig(cfg)}
	staticServer := &Server{port: listenPort, server: server, ln: ln}
	go server.Serve(ln)
	go func() {
		<-ctx.Done()
		staticServer.Close()
	}()
	return staticServer, nil
}

func listenerPort(ln net.Listener) (int, error) {
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("listener address %s is not TCP", ln.Addr())
	}
	return addr.Port, nil
}

func isHTMLFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".html" || ext == ".htm"
}

func serveLiveHTML(w http.ResponseWriter, r *http.Request, fullPath string, info os.FileInfo) {
	data, err := os.ReadFile(fullPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body := injectLiveClient(data)
	http.ServeContent(w, r, filepath.Base(fullPath), info.ModTime(), bytes.NewReader(body))
}

func injectLiveClient(data []byte) []byte {
	index := lastIndexASCIIFold(data, []byte("</body>"))
	if index < 0 {
		return append(append([]byte(nil), data...), []byte(liveClientScript)...)
	}
	out := make([]byte, 0, len(data)+len(liveClientScript))
	out = append(out, data[:index]...)
	out = append(out, liveClientScript...)
	out = append(out, data[index:]...)
	return out
}

func lastIndexASCIIFold(data, needle []byte) int {
	if len(needle) == 0 {
		return len(data)
	}
	for i := len(data) - len(needle); i >= 0; i-- {
		if equalASCIIFold(data[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

func equalASCIIFold(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

type liveBroker struct {
	mu      sync.Mutex
	clients map[chan struct{}]struct{}
}

func newLiveBroker() *liveBroker {
	return &liveBroker{clients: make(map[chan struct{}]struct{})}
}

func (b *liveBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := b.subscribe()
	defer b.unsubscribe(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprint(w, "data: reload\n\n")
			flusher.Flush()
		}
	}
}

func (b *liveBroker) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *liveBroker) unsubscribe(ch chan struct{}) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
}

func (b *liveBroker) broadcast() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

type fileState struct {
	size    int64
	modTime int64
}

func watchStaticRoot(ctx context.Context, root string, interval time.Duration, previous map[string]fileState, broker *liveBroker) {
	if interval == 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := snapshotStaticRoot(root)
			if err != nil {
				continue
			}
			if !sameSnapshot(previous, current) {
				previous = current
				broker.broadcast()
			}
		}
	}
}

func snapshotStaticRoot(root string) (map[string]fileState, error) {
	out := make(map[string]fileState)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if entry.IsDir() {
			if path != root && ignoredWatchDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = fileState{size: info.Size(), modTime: info.ModTime().UnixNano()}
		return nil
	})
	return out, err
}

func ignoredWatchDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".cache", ".parcel-cache", ".next", ".nuxt", ".turbo", "coverage":
		return true
	default:
		return false
	}
}

func sameSnapshot(a, b map[string]fileState) bool {
	if len(a) != len(b) {
		return false
	}
	for path, left := range a {
		if right, ok := b[path]; !ok || right != left {
			return false
		}
	}
	return true
}

func (s *Server) Port() int {
	if s == nil {
		return 0
	}
	return s.port
}

func (s *Server) PortString() string {
	return strconv.Itoa(s.Port())
}

func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	if s.server != nil {
		s.server.Close()
	}
	if s.ln != nil {
		s.ln.Close()
	}
	return nil
}

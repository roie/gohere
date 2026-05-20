package staticserver

import (
	"context"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

type Server struct {
	port   int
	server *http.Server
	ln     net.Listener
}

func IsStaticProject(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "index.html"))
	return err == nil && !info.IsDir()
}

func Handler(root string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		http.ServeFile(w, r, fullPath)
	})
}

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
	if host == "" {
		host = "127.0.0.1"
	}
	addr := host + ":"
	if port != 0 {
		addr += strconv.Itoa(port)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	server := &http.Server{Handler: Handler(dir)}
	staticServer := &Server{port: ln.Addr().(*net.TCPAddr).Port, server: server, ln: ln}
	go server.Serve(ln)
	go func() {
		<-ctx.Done()
		staticServer.Close()
	}()
	return staticServer, nil
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

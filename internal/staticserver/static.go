package staticserver

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
		path := filepath.Clean(r.URL.Path)
		if path == "." || path == "/" {
			path = "index.html"
		} else {
			path = path[1:]
		}

		fullPath := filepath.Join(root, path)
		info, err := os.Stat(fullPath)
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, fullPath)
	})
}

func Start(ctx context.Context, dir string, port int) (*Server, error) {
	addr := "127.0.0.1:"
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

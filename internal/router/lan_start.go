package router

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

type LANIngress struct {
	httpServer  *http.Server
	httpsServer *http.Server
	httpLn      net.Listener
	httpsLn     net.Listener
	stopContext func() bool
	closeOnce   sync.Once
	closeErr    error
}

func StartLANIngress(ctx context.Context, server *Server, httpAddr, httpsAddr string) (*LANIngress, error) {
	if server == nil {
		return nil, errors.New("LAN router is required")
	}
	if httpAddr == "" || httpsAddr == "" {
		return nil, errors.New("LAN HTTP and HTTPS addresses are required")
	}
	httpListener, err := net.Listen("tcp", httpAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on LAN HTTP address %s: %w", httpAddr, err)
	}
	httpsListener, err := net.Listen("tcp", httpsAddr)
	if err != nil {
		httpListener.Close()
		return nil, fmt.Errorf("listen on LAN HTTPS address %s: %w", httpsAddr, err)
	}
	ingress := &LANIngress{
		httpServer: &http.Server{
			Handler: server.LANHTTPHandler(), ReadHeaderTimeout: proxyReadHeaderTimeout,
		},
		httpsServer: &http.Server{
			Handler: server.LANHandler(), TLSConfig: server.LANTLSConfig(), ReadHeaderTimeout: proxyReadHeaderTimeout,
		},
		httpLn: httpListener, httpsLn: httpsListener,
	}
	ingress.stopContext = context.AfterFunc(ctx, func() { _ = ingress.Close() })
	go ingress.httpServer.Serve(httpListener)
	go ingress.httpsServer.Serve(tls.NewListener(httpsListener, ingress.httpsServer.TLSConfig))
	return ingress, nil
}

func (i *LANIngress) HTTPAddr() string {
	if i == nil || i.httpLn == nil {
		return ""
	}
	return i.httpLn.Addr().String()
}

func (i *LANIngress) HTTPSAddr() string {
	if i == nil || i.httpsLn == nil {
		return ""
	}
	return i.httpsLn.Addr().String()
}

func (i *LANIngress) Close() error {
	if i == nil {
		return nil
	}
	i.closeOnce.Do(func() {
		if i.stopContext != nil {
			i.stopContext()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := i.httpServer.Shutdown(ctx); err != nil {
			i.closeErr = err
		}
		if err := i.httpsServer.Shutdown(ctx); err != nil && i.closeErr == nil {
			i.closeErr = err
		}
		_ = i.httpLn.Close()
		_ = i.httpsLn.Close()
	})
	return i.closeErr
}

package app

import (
	"context"
	"errors"
	"io"
	"runtime"

	appconfig "github.com/roie/gohere/internal/config"
	"github.com/roie/gohere/internal/router"
	"github.com/roie/gohere/internal/tunnel"
)

type WindowsTunnelConfig struct {
	GOOS      string
	HTTPS     *bool
	Dial      tunnel.DialContextFunc
	LogOutput io.Writer
}

func ServeWindowsTunnel(ctx context.Context, input io.Reader, output io.Writer, cfg WindowsTunnelConfig) error {
	goos := cfg.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos != "windows" {
		return errors.New("gohere tunnel helper requires Windows")
	}
	https := false
	if cfg.HTTPS != nil {
		https = *cfg.HTTPS
	} else if state, err := appconfig.Load(router.DefaultStateDir()); err == nil {
		https = state.HTTPS
	}
	closer := &tunnelStdioCloser{}
	closer.input, _ = input.(io.Closer)
	closer.output, _ = output.(io.Closer)
	conn := &tunnel.PipeConn{Reader: input, Writer: output, Closer: closer}
	return tunnel.Serve(ctx, conn, tunnel.ServerConfig{
		HTTPS:     https,
		Dial:      cfg.Dial,
		LogOutput: cfg.LogOutput,
	})
}

type tunnelStdioCloser struct {
	input  io.Closer
	output io.Closer
}

func (c *tunnelStdioCloser) Close() error {
	if c == nil {
		return nil
	}
	var err error
	if c.input != nil {
		err = errors.Join(err, c.input.Close())
	}
	if c.output != nil {
		err = errors.Join(err, c.output.Close())
	}
	return err
}

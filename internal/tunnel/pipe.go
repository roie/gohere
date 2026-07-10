package tunnel

import (
	"io"
	"net"
	"sync"
	"time"
)

type PipeConn struct {
	Reader io.Reader
	Writer io.Writer
	Closer io.Closer

	closeOnce sync.Once
	closeErr  error
}

func (c *PipeConn) Read(data []byte) (int, error)  { return c.Reader.Read(data) }
func (c *PipeConn) Write(data []byte) (int, error) { return c.Writer.Write(data) }

func (c *PipeConn) Close() error {
	c.closeOnce.Do(func() {
		if c.Closer != nil {
			c.closeErr = c.Closer.Close()
		}
	})
	return c.closeErr
}

func (*PipeConn) LocalAddr() net.Addr              { return pipeAddress("stdio-local") }
func (*PipeConn) RemoteAddr() net.Addr             { return pipeAddress("stdio-remote") }
func (*PipeConn) SetDeadline(time.Time) error      { return nil }
func (*PipeConn) SetReadDeadline(time.Time) error  { return nil }
func (*PipeConn) SetWriteDeadline(time.Time) error { return nil }

type pipeAddress string

func (pipeAddress) Network() string  { return "stdio" }
func (a pipeAddress) String() string { return string(a) }

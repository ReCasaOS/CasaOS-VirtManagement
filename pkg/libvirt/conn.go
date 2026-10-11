// Package libvirt keeps the service's connection to libvirtd.
package libvirt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
	"go.uber.org/zap"
)

const (
	// RetryInterval is how long to wait before dialling libvirtd again after a
	// failed attempt or a lost connection.
	RetryInterval = 10 * time.Second

	dialTimeout = 5 * time.Second
)

// ErrUnavailable is returned while there is no connection to libvirtd. The
// wrapping error says why.
var ErrUnavailable = errors.New("libvirt unavailable")

// Conn is one connection to libvirtd, redialled whenever it drops.
//
// The service does not exit when libvirt is missing: it keeps serving the API,
// reports libvirt as unavailable, and tries again every RetryInterval.
// Crash-looping under Restart=always on a box without libvirt would only fill
// the journal.
type Conn struct {
	sockets   []string
	uri       string
	onConnect []func(ctx context.Context, l *golibvirt.Libvirt)

	mu     sync.RWMutex
	l      *golibvirt.Libvirt
	socket string
	reason string
}

// New returns a connection that will try sockets in order and ask the daemon
// for uri, qemu:///system when empty. Nothing is dialled until Run.
func New(sockets []string, uri string) *Conn {
	return &Conn{
		sockets: sockets,
		uri:     uri,
		reason:  "not connected yet",
	}
}

// OnConnect registers f to run, in a goroutine of its own, every time a
// connection is made. Its ctx is cancelled when that connection is lost. Call
// it before Run.
func (c *Conn) OnConnect(f func(ctx context.Context, l *golibvirt.Libvirt)) {
	c.onConnect = append(c.onConnect, f)
}

// Get returns the live connection, or an error wrapping ErrUnavailable.
func (c *Conn) Get() (*golibvirt.Libvirt, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.l == nil || !c.l.IsConnected() {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, c.reason)
	}

	return c.l, nil
}

// Socket is the socket of the live connection, empty when there is none.
func (c *Conn) Socket() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.socket
}

// Run connects, and reconnects after every loss, until ctx is done.
func (c *Conn) Run(ctx context.Context) {
	for {
		l, socket, err := c.dial()
		if err != nil {
			c.set(nil, "", err.Error())
			logger.Error("libvirt is unavailable, retrying", zap.Error(err), zap.Duration("in", RetryInterval))

			select {
			case <-ctx.Done():
				return
			case <-time.After(RetryInterval):
				continue
			}
		}

		c.set(l, socket, "")
		logger.Info("connected to libvirt", zap.String("socket", socket))

		connCtx, cancelConn := context.WithCancel(ctx)
		for _, f := range c.onConnect {
			go f(connCtx, l)
		}

		select {
		case <-ctx.Done():
			cancelConn()
			c.set(nil, "", "shutting down")
			if err := l.Disconnect(); err != nil {
				logger.Error("error when disconnecting from libvirt", zap.Error(err))
			}
			return

		case <-l.Disconnected():
			cancelConn()
			c.set(nil, "", "connection to "+socket+" lost")
			logger.Error("lost the connection to libvirt, reconnecting", zap.String("socket", socket))
		}
	}
}

func (c *Conn) set(l *golibvirt.Libvirt, socket, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.l, c.socket, c.reason = l, socket, reason
}

// dial tries each socket in turn. A socket that does not exist is skipped, so
// the reason reported for a box without libvirt is that it is not installed,
// not a dial error.
func (c *Conn) dial() (*golibvirt.Libvirt, string, error) {
	var failures []string

	for _, socket := range c.sockets {
		if _, err := os.Stat(socket); err != nil {
			failures = append(failures, socket+": not found")
			continue
		}

		l := golibvirt.NewWithDialer(dialers.NewLocal(dialers.WithSocket(socket), dialers.WithLocalTimeout(dialTimeout)))
		if err := c.connect(l); err != nil {
			failures = append(failures, socket+": "+err.Error())
			continue
		}

		return l, socket, nil
	}

	return nil, "", errors.New(strings.Join(failures, "; "))
}

func (c *Conn) connect(l *golibvirt.Libvirt) error {
	if c.uri == "" {
		return l.Connect()
	}

	return l.ConnectToURI(golibvirt.ConnectURI(c.uri))
}

package server

import (
	"encoding/base64"
	"fmt"
	"math/rand"
	"net"
	"ngrok/conn"
	"ngrok/log"
	"ngrok/msg"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var defaultPortMap = map[string]int{
	"http":  80,
	"https": 443,
	"smtp":  25,
}

/**
 * Tunnel: A control connection, metadata and proxy connections which
 *         route public traffic to a firewalled endpoint.
 */
type Tunnel struct {
	// request that opened the tunnel
	req *msg.ReqTunnel

	// time when the tunnel was opened
	start time.Time

	// public url
	url string

	// tcp listener
	listener *net.TCPListener

	// control connection
	ctl *Control

	// logger
	log.Logger

	// closing
	closing int32
}

// Common functionality for registering virtually hosted protocols
func registerVhost(t *Tunnel, protocol string, servingPort int) (err error) {
	vhost := os.Getenv("VHOST")
	if vhost == "" {
		vhost = fmt.Sprintf("%s:%d", opts.domain, servingPort)
	}

	// Canonicalize virtual host by removing default port (e.g. :80 on HTTP)
	defaultPort, ok := defaultPortMap[protocol]
	if !ok {
		return fmt.Errorf("Couldn't find default port for protocol %s", protocol)
	}

	defaultPortSuffix := fmt.Sprintf(":%d", defaultPort)
	if strings.HasSuffix(vhost, defaultPortSuffix) {
		vhost = vhost[0 : len(vhost)-len(defaultPortSuffix)]
	}

	// Canonicalize by always using lower-case
	vhost = strings.ToLower(vhost)

	// Register for specific hostname
	hostname := strings.ToLower(strings.TrimSpace(t.req.Hostname))
	if hostname != "" {
		t.url = fmt.Sprintf("%s://%s", protocol, hostname)
		return tunnelRegistry.Register(t.url, t)
	}

	// Register for specific subdomain
	subdomain := strings.ToLower(strings.TrimSpace(t.req.Subdomain))
	if subdomain != "" {
		t.url = fmt.Sprintf("%s://%s.%s", protocol, subdomain, vhost)
		return tunnelRegistry.Register(t.url, t)
	}

	// Register for random URL
	t.url, err = tunnelRegistry.RegisterRepeat(func() string {
		return fmt.Sprintf("%s://%x.%s", protocol, rand.Int31(), vhost)
	}, t)

	return
}

// Create a new tunnel from a registration message received
// on a control channel
func NewTunnel(m *msg.ReqTunnel, ctl *Control) (t *Tunnel, err error) {
	t = &Tunnel{
		req:    m,
		start:  time.Now(),
		ctl:    ctl,
		Logger: log.NewPrefixLogger(),
	}

	proto := t.req.Protocol
	switch proto {
	case "tcp":
		var port int = 0

		// try to return to you the same port you had before
		cachedUrl := tunnelRegistry.GetCachedRegistration(t)
		if cachedUrl != "" {
			parts := strings.Split(cachedUrl, ":")
			portPart := parts[len(parts)-1]
			port, err = strconv.Atoi(portPart)
			if err != nil {
				t.ctl.conn.Error("Failed to parse cached url port as integer: %s", portPart)
				// continue with zero
				port = 0
			}
		}

		// Bind for TCP connections
		t.listener, err = net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: port})

		// If we failed with a custom port, try with a random one
		if err != nil && port != 0 {
			t.ctl.conn.Warn("Failed to get custom port %d: %v, trying a random one", port, err)
			t.listener, err = net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 0})
		}

		// we tried to bind with a random port and failed (no more ports available?)
		if err != nil {
			err = t.ctl.conn.Error("Error binding TCP listener: %v", err)
			return
		}

		// create the url
		addr := t.listener.Addr().(*net.TCPAddr)
		t.url = fmt.Sprintf("tcp://%s:%d", opts.domain, addr.Port)

		// register it
		if err = tunnelRegistry.RegisterAndCache(t.url, t); err != nil {
			// This should never be possible because the OS will
			// only assign available ports to us.
			t.listener.Close()
			err = fmt.Errorf("TCP listener bound, but failed to register %s", t.url)
			return
		}

		go t.listenTcp(t.listener)

	case "http", "https":
		l, ok := listeners[proto]
		if !ok {
			err = fmt.Errorf("Not listeneing for %s connections", proto)
			return
		}

		if err = registerVhost(t, proto, l.Addr.(*net.TCPAddr).Port); err != nil {
			return
		}

	default:
		err = fmt.Errorf("Protocol %s is not supported", proto)
		return
	}

	// pre-encode the http basic auth for fast comparisons later
	if m.HttpAuth != "" {
		m.HttpAuth = "Basic " + base64.StdEncoding.EncodeToString([]byte(m.HttpAuth))
	}

	t.AddLogPrefix(t.Id())
	t.Info("Registered new tunnel on: %s", t.ctl.conn.Id())

	metrics.OpenTunnel(t)
	return
}

func (t *Tunnel) Shutdown() {
	t.Info("Shutting down")

	// mark that we're shutting down
	atomic.StoreInt32(&t.closing, 1)

	// if we have a public listener (this is a raw TCP tunnel), shut it down
	if t.listener != nil {
		t.listener.Close()
	}

	// remove ourselves from the tunnel registry
	tunnelRegistry.Del(t.url)

	// let the control connection know we're shutting down
	// currently, only the control connection shuts down tunnels,
	// so it doesn't need to know about it
	// t.ctl.stoptunnel <- t

	metrics.CloseTunnel(t)
}

func (t *Tunnel) Id() string {
	return t.url
}

// Listens for new public tcp connections from the internet.
func (t *Tunnel) listenTcp(listener *net.TCPListener) {
	for {
		defer func() {
			if r := recover(); r != nil {
				log.Warn("listenTcp failed with error %v", r)
			}
		}()

		// accept public connections
		tcpConn, err := listener.AcceptTCP()

		if err != nil {
			// not an error, we're shutting down this tunnel
			if atomic.LoadInt32(&t.closing) == 1 {
				return
			}

			t.Error("Failed to accept new TCP connection: %v", err)
			continue
		}

		conn := conn.Wrap(tcpConn, "pub")
		conn.AddLogPrefix(t.Id())
		conn.Info("New connection from %v", conn.RemoteAddr())

		go t.HandlePublicConnection(conn)
	}
}

func (t *Tunnel) HandlePublicConnection(publicConn conn.Conn) {
	defer publicConn.Close()
	defer func() {
		if r := recover(); r != nil {
			publicConn.Warn("HandlePublicConnection failed with error %v", r)
		}
	}()

	startTime := time.Now()
	metrics.OpenConnection(t, publicConn)

	var proxyConn conn.Conn
	var attempts int
	var err error
	for {
		// get a proxy connection
		if proxyConn, err = t.ctl.GetProxy(); err != nil {
			t.Warn("Failed to get proxy connection: %v", err)
			return
		}
		defer proxyConn.Close()
		t.Info("Got proxy connection %s", proxyConn.Id())
		proxyConn.AddLogPrefix(t.Id())

		// tell the client we're going to start using this proxy connection
		startPxyMsg := &msg.StartProxy{
			Url:        t.url,
			ClientAddr: publicConn.RemoteAddr().String(),
		}
		if err = msg.WriteMsg(proxyConn, startPxyMsg); err != nil {
			attempts += 1
			proxyConn.Warn("Failed to write StartProxyMessage: %v, attempt %d", err, attempts)
			if attempts > 3 {
				// give up
				publicConn.Error("Too many failures starting proxy connection")
				return
			}
		} else {
			// success
			break
		}
	}

	// join the public and proxy connections
	bytesIn, bytesOut := conn.Join(publicConn, proxyConn)
	metrics.CloseConnection(t, publicConn, startTime, bytesIn, bytesOut)
}

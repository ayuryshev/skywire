package skymsg

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"

	"github.com/skycoin/skycoin/src/util/logging"
	"github.com/skycoin/skywire/internal/noise"
	"github.com/skycoin/skywire/pkg/cipher"
	"github.com/skycoin/skywire/pkg/messaging-discovery/client"
	"github.com/skycoin/skywire/pkg/transport"
)

var (
	// ErrNoSrv indicate that remote client does not have DelegatedServers in entry.
	ErrNoSrv = errors.New("remote has no DelegatedServers")
	// ErrRejected indicates that ChannelOpen frame was rejected by remote server.
	ErrRejected = errors.New("rejected")
	// ErrChannelClosed indicates that underlying channel is being closed and writes are prohibited.
	ErrChannelClosed = errors.New("channel closed")
	// ErrDeadlineExceeded indicates that read/write operation failed due to timeout.
	ErrDeadlineExceeded = errors.New("deadline exceeded")
	// ErrClientClosed indicates that client is closed and not accepting new connections.
	ErrClientClosed = errors.New("client closed")
)

var log = logging.NewMasterLogger()

// Link from a client's perspective.
type Link struct {
	net.Conn                           // conn to skymsg server
	local     cipher.PubKey            // local client's pk
	remoteSrv cipher.PubKey            // skymsg server's public key
	nextID    uint16                   // next unused channel ID
	chans     [math.MaxUint16]*Channel // channels to skymsg clients
	mx        sync.RWMutex
	wg        sync.WaitGroup
}

func NewLink(conn net.Conn, remote cipher.PubKey) *Link {
	return &Link{Conn: conn, remoteSrv: remote, nextID: 0}
}

func (l *Link) delChan(id uint16) {
	l.mx.Lock()
	l.chans[id] = nil
	l.mx.Unlock()
}

func (l *Link) setChan(id uint16, ch *Channel) *Channel {
	l.mx.Lock()
	l.chans[id] = ch
	l.mx.Unlock()
	return ch
}

func (l *Link) addChan(ctx context.Context, clientPK cipher.PubKey) (*Channel, error) {
	l.mx.Lock()
	defer l.mx.Unlock()

	for {
		if ch := l.chans[l.nextID]; ch == nil || ch.IsDone() {
			break
		}
		l.nextID += 2

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}

	id := l.nextID
	l.nextID = id + 2
	return NewChannel(l.Conn, l.local, clientPK, id), nil
}

func (l *Link) getChan(id uint16) (*Channel, bool) {
	l.mx.RLock()
	ch := l.chans[id]
	l.mx.RUnlock()
	ok := ch != nil && !ch.IsDone()
	return ch, ok
}

func checkRequest(local cipher.PubKey, id uint16, p []byte) (remote cipher.PubKey, ok bool) {
	// server-initiated channels should have odd channel ID
	if isEven(id) {
		return cipher.PubKey{}, false
	}

	// check expected request payload
	initPK, respPK, ok := splitPKs(p)
	if !ok || respPK != local {
		return cipher.PubKey{}, false
	}

	return initPK, true
}

func (l *Link) Serve(ctx context.Context, accept chan<- *Channel) error {
	l.wg.Add(1)
	defer l.wg.Done()

	for {
		f, err := readFrame(l.Conn)
		if err != nil {
			return err
		}

		ft, id, p := f.Disassemble()
		ch, ok := l.getChan(id)

		if !ok {
			l.delChan(id)

			switch ft {
			case RequestType:
				remote, ok := checkRequest(l.local, id, p)
				if !ok {
					_, _ = l.Conn.Write(MakeFrame(CloseType, id, []byte{0}))
					continue
				}
				accept <- l.setChan(id, NewChannel(l.Conn, l.local, remote, id))

			case CloseType:
				continue

			default:
				_, _ = l.Conn.Write(MakeFrame(CloseType, id, []byte{0}))
				continue
			}
		}

		if !ch.AwaitRead(f) {
			l.delChan(id)
		}
	}
}

func (l *Link) Dial(ctx context.Context, clientPK cipher.PubKey) (*Channel, error) {
	ch, err := l.addChan(ctx, clientPK)
	if err != nil {
		return nil, err
	}
	return ch, ch.Handshake(ctx)
}

func (l *Link) Close() error {
	l.mx.Lock()
	for _, ch := range l.chans {
		if ch != nil {
			_ = ch.Close()
		}
	}
	err := l.Conn.Close()
	l.mx.Unlock()
	l.wg.Wait()
	return err
}

type Client struct {
	pk cipher.PubKey
	sk cipher.SecKey
	dc client.APIClient

	links map[cipher.PubKey]*Link // links with messaging servers. Key: pk of server
	mx    sync.RWMutex

	accept chan *Channel
	once   sync.Once
}

func NewClient(pk cipher.PubKey, sk cipher.SecKey, dc client.APIClient) *Client {
	return &Client{
		pk:     pk,
		sk:     sk,
		dc:     dc,
		links:  make(map[cipher.PubKey]*Link),
		accept: make(chan *Channel),
	}
}

func (c *Client) setLink(l *Link) {
	c.mx.Lock()
	c.links[l.remoteSrv] = l
	c.mx.Unlock()
}

func (c *Client) delLink(pk cipher.PubKey) {
	c.mx.Lock()
	delete(c.links, pk)
	c.mx.Unlock()
}

func (c *Client) getLink(pk cipher.PubKey) (*Link, bool) {
	c.mx.RLock()
	l, ok := c.links[pk]
	c.mx.RUnlock()
	return l, ok
}

func (c *Client) newLink(ctx context.Context, srvPK cipher.PubKey, addr string) (*Link, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	ns, err := noise.New(noise.HandshakeXK, noise.Config{
		LocalPK:   c.pk,
		LocalSK:   c.sk,
		RemotePK:  srvPK,
		Initiator: true,
	})
	if err != nil {
		return nil, err
	}
	nc, err := noise.WrapConn(conn, ns, hsTimeout)
	if err != nil {
		return nil, err
	}
	l := NewLink(nc, srvPK)
	go func() {
		if err := l.Serve(ctx, c.accept); err != nil {
			log.WithError(err).WithField("srv_pk", l.remoteSrv).Warn("link with server closed")
			c.delLink(l.remoteSrv)
		}
	}()
	return l, nil
}

func (c *Client) InitiateLinks(ctx context.Context, n int) error {
	c.mx.Lock()
	defer c.mx.Unlock()

	entries, err := c.dc.AvailableServers(ctx)
	if err != nil {
		return fmt.Errorf("messaging servers are not available: %s", err)
	}
	for _, entry := range entries {
		if len(c.links) > n {
			break
		}
		link, err := c.newLink(ctx, entry.Static, entry.Server.Address)
		if err != nil {
			log.Warnf("Failed to connect to server %s: %s", entry.Static, err)
			continue
		}
		c.links[link.remoteSrv] = link
	}
	if len(c.links) == 0 {
		return fmt.Errorf("servers are not available: all servers failed")
	}
	if err := c.updateDiscEntry(ctx); err != nil {
		return fmt.Errorf("updating client's discovery entry failed with: %s", err)
	}
	return nil
}

func (c *Client) findLink(ctx context.Context, srvPKs []cipher.PubKey) (*Link, error) {
	for _, srvPK := range srvPKs {
		if l, ok := c.links[srvPK]; !ok {
			return l, nil
		}
	}
	for _, srvPK := range srvPKs {
		entry, err := c.dc.Entry(ctx, srvPK)
		if err != nil {
			return nil, fmt.Errorf("get server failure: %s", err)
		}
		link, err := c.newLink(ctx, entry.Static, entry.Server.Address)
		if err != nil {
			log.Warnf("Failed to connect to server %s: %s", entry.Static, err)
			continue
		}
		c.links[link.remoteSrv] = link
		return link, nil
	}
	return nil, ErrNoSrv
}

func (c *Client) updateDiscEntry(ctx context.Context) error {
	log.Info("updating client's discovery entry...")
	var srvPKs []cipher.PubKey
	c.mx.RLock()
	for pk := range c.links {
		srvPKs = append(srvPKs, pk)
	}
	c.mx.RUnlock()
	entry, err := c.dc.Entry(ctx, c.pk)
	if err != nil {
		entry = client.NewClientEntry(c.pk, 0, srvPKs)
		if err := entry.Sign(c.sk); err != nil {
			return err
		}
		return c.dc.SetEntry(ctx, entry)
	}
	entry.Client.DelegatedServers = srvPKs
	return c.dc.UpdateEntry(ctx, c.sk, entry)
}

func (c *Client) Accept(ctx context.Context) (transport.Transport, error) {
	select {
	case ch, ok := <-c.accept:
		if !ok {
			return nil, ErrClientClosed
		}
		return ch, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) Dial(ctx context.Context, remoteClient cipher.PubKey) (transport.Transport, error) {
	c.mx.Lock()
	defer c.mx.Unlock()

	entry, err := c.dc.Entry(ctx, remoteClient)
	if err != nil {
		return nil, fmt.Errorf("get entry failure: %s", err)
	}

	if len(entry.Client.DelegatedServers) == 0 {
		return nil, ErrNoSrv
	}
	link, err := c.findLink(ctx, entry.Client.DelegatedServers)
	if err != nil {
		return nil, err
	}
	return link.Dial(ctx, remoteClient)
}

func (c *Client) Local() cipher.PubKey {
	return c.pk
}

func (c *Client) Type() string {
	return "skymsg"
}

// TODO(evaninjin): proper error handling.
func (c *Client) Close() error {
	c.mx.Lock()
	defer c.mx.Unlock()

	for _, link := range c.links {
		_ = link.Close()
	}
	c.links = make(map[cipher.PubKey]*Link)
	c.once.Do(func() {
		close(c.accept)
	})
	return nil
}
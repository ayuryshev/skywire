// Package router implements package router for skywire visor.
package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/skycoin/dmsg/cipher"
	"github.com/skycoin/skycoin/src/util/logging"

	"github.com/alecthomas/repr"

	th "github.com/skycoin/skywire/internal/testhelpers"
	"github.com/skycoin/skywire/pkg/app"
	routeFinder "github.com/skycoin/skywire/pkg/route-finder/client"
	"github.com/skycoin/skywire/pkg/routing"
	"github.com/skycoin/skywire/pkg/setup"
	"github.com/skycoin/skywire/pkg/transport"
	"github.com/skycoin/skywire/pkg/transport/dmsg"
)

const (
	// RouteTTL is the default expiration interval for routes
	RouteTTL = 2 * time.Hour
	minHops  = 0
	maxHops  = 50
)

var log = logging.MustGetLogger("router")

// Config configures Router.
type Config struct {
	Logger *logging.Logger
	PubKey cipher.PubKey
	SecKey cipher.SecKey

	TransportManager *transport.Manager
	RoutingTable     routing.Table
	RouteFinder      routeFinder.Client
	SetupNodes       []cipher.PubKey
	// TransportType    string
}

// PacketRouter performs routing of the skywire packets.
type PacketRouter interface {
	io.Closer
	Serve(ctx context.Context) error
	ServeApp(conn net.Conn, port routing.Port, appConf *app.Config) error
	IsSetupTransport(tr *transport.ManagedTransport) bool
	CreateLoop(conn *app.Protocol, raddr routing.Addr) (laddr routing.Addr, err error)
	CloseLoop(conn *app.Protocol, loop routing.AddressPair) error
	ForwardAppPacket(conn *app.Protocol, packet *app.Packet) error
}

// Router implements PacketRouter. It manages routing table by
// communicating with setup nodes, forward packets according to local
// rules and manages loops for apps.
type Router struct {
	Logger *logging.Logger

	config *Config
	tm     *transport.Manager
	pm     *portManager
	rm     *routeManager

	expiryTicker *time.Ticker
	wg           sync.WaitGroup

	staticPorts map[routing.Port]struct{}
	mu          sync.Mutex
}

// New constructs a new Router.
func New(config *Config) *Router {
	r := &Router{
		Logger:       config.Logger,
		tm:           config.TransportManager,
		pm:           newPortManager(10, config.Logger),
		config:       config,
		expiryTicker: time.NewTicker(10 * time.Minute),
		staticPorts:  make(map[routing.Port]struct{}),
	}
	callbacks := &setupCallbacks{
		ConfirmLoop: r.confirmLoop,
		LoopClosed:  r.loopClosed,
	}
	r.rm = &routeManager{r.Logger, manageRoutingTable(config.RoutingTable), callbacks}
	return r
}

func (r *Router) trLog() *logrus.Entry {
	return r.Logger.WithField("func", th.GetCallerN(4))
}
func (r *Router) trStart() (_ error) {
	r.trLog().Info("ENTER")
	return
}
func (r *Router) trFinish(_ error) {
	r.trLog().Info("EXIT")
	return
}

// Serve starts transport listening loop.
func (r *Router) Serve(ctx context.Context) error {
	defer r.trFinish(r.trStart())
	r.Logger.Info("Starting router")

	go func() {
		for {
			select {
			case dTp, ok := <-r.tm.DataTpChan:
				if !ok {
					return
				}
				initStatus := "locally"
				if dTp.Accepted {
					initStatus = "remotely"
				}
				r.Logger.Infof("New %s-initiated transport: purpose(data)", initStatus)
				r.handleTransport(dTp, dTp.Accepted, false)

			case sTp, ok := <-r.tm.SetupTpChan:
				if !ok {
					return
				}
				r.Logger.Infof("New remotely-initiated transport: purpose(setup)")
				r.handleTransport(sTp, true, true)

			case <-r.expiryTicker.C:
				if err := r.rm.rt.Cleanup(); err != nil {
					r.Logger.Warnf("Failed to expiry routes: %s", err)
				}
			}
		}
	}()
	return r.tm.Serve(ctx)
}

func (r *Router) handleTransport(tp transport.Transport, isAccepted, isSetup bool) {
	defer r.trFinish(r.trStart())
	r.Logger.Info("Starting router")

	var serve func(io.ReadWriter) error
	switch {
	case isAccepted && isSetup:
		serve = r.rm.Serve
	case !isSetup:
		serve = r.serveTransport
	default:
		return
	}

	go func(tp transport.Transport) {
		defer func() {
			if err := tp.Close(); err != nil {
				r.Logger.Warnf("Failed to close transport: %s", err)
			}
		}()
		for {
			if err := serve(tp); err != nil {
				if err != io.EOF {
					r.Logger.Warnf("Stopped serving Transport: %s", err)
				}
				return
			}
		}
	}(tp)
}

// ServeApp handles App packets from the App connection on provided port.
func (r *Router) ServeApp(conn net.Conn, port routing.Port, appConf *app.Config) error {
	defer r.trFinish(r.trStart())

	r.Logger.Infof("%v Starting router", th.GetCaller())

	r.wg.Add(1)
	defer r.wg.Done()

	appProto := app.NewProtocol(conn)
	if err := r.pm.Open(port, appProto); err != nil {
		return err
	}

	r.mu.Lock()
	r.staticPorts[port] = struct{}{}
	r.mu.Unlock()

	am := &appManager{r.Logger, r, appProto, appConf}
	err := am.Serve()

	for _, port := range r.pm.AppPorts(appProto) {
		for _, addr := range r.pm.Close(port) {
			if err := r.closeLoop(routing.AddressPair{Local: routing.Addr{Port: port}, Remote: addr}); err != nil {
				log.WithError(err).Warn("Failed to close loop")
			}
		}
	}

	r.mu.Lock()
	delete(r.staticPorts, port)
	r.mu.Unlock()

	if err == io.EOF {
		return nil
	}
	return err
}

// CreateLoop implements PactetRouter.CreateLoop
func (r *Router) CreateLoop(conn *app.Protocol, raddr routing.Addr) (laddr routing.Addr, err error) {
	defer r.trFinish(r.trStart())
	return r.requestLoop(conn, raddr)
}

// CloseLoop implements PactetRouter.CloseLoop
func (r *Router) CloseLoop(_ *app.Protocol, loop routing.AddressPair) error {
	defer r.trFinish(r.trStart())
	return r.closeLoop(loop)
}

// ForwardAppPacket implements PactetRouter.ForwardAppPacket
func (r *Router) ForwardAppPacket(_ *app.Protocol, packet *app.Packet) error {
	defer r.trFinish(r.trStart())
	return r.forwardAppPacket(packet)
}

// Close safely stops Router.
func (r *Router) Close() error {
	defer r.trFinish(r.trStart())

	if r == nil {
		return nil
	}

	r.Logger.Info("Closing all App connections and Loops")
	r.expiryTicker.Stop()

	for _, conn := range r.pm.AppConns() {
		if err := conn.Close(); err != nil {
			log.WithError(err).Warn("Failed to close connection")
		}
	}

	r.wg.Wait()
	return r.tm.Close()
}

func (r *Router) serveTransport(rw io.ReadWriter) error {
	defer r.trFinish(r.trStart())

	packet := make(routing.Packet, 6)
	if _, err := io.ReadFull(rw, packet); err != nil {
		return err
	}

	payload := make([]byte, packet.Size())
	if _, err := io.ReadFull(rw, payload); err != nil {
		return err
	}

	rule, err := r.rm.GetRule(packet.RouteID())
	if err != nil {
		return err
	}

	r.Logger.Infof("Got new remote packet with route ID %d. Using rule: %s", packet.RouteID(), rule)
	if rule.Type() == routing.RuleForward {
		return r.ForwardPacket(payload, rule)
	}

	return r.consumePacket(payload, rule)
}

// ForwardPacket forwards payload according to rule
func (r *Router) ForwardPacket(payload []byte, rule routing.Rule) error {
	defer r.trFinish(r.trStart())

	r.Logger.Infof("%v %v %v\n", th.GetCaller(), payload, rule)

	packet := routing.MakePacket(rule.RouteID(), payload)
	tr := r.tm.Transport(rule.TransportID())
	if tr == nil {
		r.Logger.Infof("%v: packet %v \n", th.GetCaller(), packet)

		// tr = r.tm.CreateDataTransport(context.Background(), ra)
		return errors.New("unknown transport")
	}

	if _, err := tr.Write(packet); err != nil {
		return err
	}

	r.Logger.Infof("Forwarded packet via Transport %s using rule %d", rule.TransportID(), rule.RouteID())
	return nil
}

func (r *Router) consumePacket(payload []byte, rule routing.Rule) error {
	defer r.trFinish(r.trStart())

	laddr := routing.Addr{Port: rule.LocalPort()}
	raddr := routing.Addr{PubKey: rule.RemotePK(), Port: rule.RemotePort()}

	p := &app.Packet{AddressPair: routing.AddressPair{Local: laddr, Remote: raddr}, Payload: payload}
	b, err := r.pm.Get(rule.LocalPort())
	if err != nil {
		return err
	}
	if err := b.conn.Send(app.FrameSend, p, nil); err != nil {
		return err
	}

	r.Logger.Infof("Forwarded packet to App on Port %d", rule.LocalPort())
	return nil
}

func reprS(v interface{}) string {
	w := bytes.NewBuffer(nil)

	options := []repr.Option{repr.OmitEmpty(true), repr.IgnoreGoStringer()}
	// options = []Option{repr.NoIndent()}
	p := repr.New(w, options...)
	p.Print(v)
	return w.String()
}

func (r *Router) forwardAppPacket(appPacket *app.Packet) error {
	defer r.trFinish(r.trStart())
	if r == nil {
		return errors.New("router not initialized")
	}

	if appPacket.AddressPair.Remote.PubKey == r.config.PubKey {
		r.trLog().Info("Forwarding local packet to: ", r.config.PubKey)
		return r.forwardLocalAppPacket(appPacket)
	}

	if r.isPubKeyLocal(appPacket.AddressPair.Remote.PubKey) {
		r.trLog().Info("Forwarding tcp-transport packet to: ", appPacket.AddressPair.Remote.PubKey)
		return r.forwardTCPAppPacket(appPacket)
	}

	r.trLog().Info("Forwarding dmsg packet to: ", appPacket.AddressPair.Remote.PubKey)
	return r.forwardLocalDMSGPacket(appPacket)
}

func (r *Router) forwardLocalDMSGPacket(appPacket *app.Packet) error {
	loop, err := r.pm.GetLoop(appPacket.AddressPair.Local.Port, appPacket.AddressPair.Remote)
	if err != nil {
		return err
	}
	tr := r.tm.Transport(loop.trID)
	if tr == nil {
		r.trLog().Info("unknown transport")
		return errors.New("unknown transport")
	}
	r.trLog().Infof("r.tm.transports: %v\n", r.tm.Transports())

	rPacket := routing.MakePacket(loop.routeID, appPacket.Payload)
	r.Logger.Infof("%v Forwarded App packet from LocalPort %d using route ID %d", th.GetCaller(), appPacket.AddressPair.Local.Port, loop.routeID)
	_, err = tr.Write(rPacket)
	if err != nil {
		r.Logger.Warnf("%v tr.Write: %v", err)
	}

	return err
}

func (r *Router) forwardLocalAppPacket(appPacket *app.Packet) error {
	defer r.trFinish(r.trStart())

	b, err := r.pm.Get(appPacket.AddressPair.Remote.Port)
	if err != nil {
		r.Logger.Warnf("%v r.pm.Get: %v\n", th.GetCaller(), err)
		return nil
	}

	p := &app.Packet{
		AddressPair: routing.AddressPair{
			Local:  routing.Addr{Port: appPacket.AddressPair.Remote.Port},
			Remote: routing.Addr{PubKey: appPacket.AddressPair.Remote.PubKey, Port: appPacket.AddressPair.Local.Port},
		},
		Payload: appPacket.Payload,
	}
	errSend := b.conn.Send(app.FrameSend, p, nil)
	if errSend != nil {
		r.Logger.Warnf("%v b.conn.Send: %v\n", th.GetCaller(), err)
	}
	return errSend
}

func (r *Router) forwardTCPAppPacket(appPacket *app.Packet) error {
	defer r.trFinish(r.trStart())

	loop, err := r.pm.GetLoop(appPacket.AddressPair.Local.Port, appPacket.AddressPair.Remote)
	if err != nil {
		return err
	}
	trID := transport.MakeTransportID(r.config.PubKey, appPacket.AddressPair.Remote.PubKey, "tcp-transport", true)
	tr := r.tm.Transport(trID)
	if tr == nil {
		r.trLog().Info("unknown transport")
		return errors.New("unknown transport")
	}
	r.trLog().Errorf("r.tm.transports: %v\n", r.tm.Transports())

	rPacket := routing.MakePacket(loop.routeID, appPacket.Payload)
	r.Logger.Infof("%v Forwarded App packet from LocalPort %d using route ID %d", th.GetCaller(), appPacket.AddressPair.Local.Port, loop.routeID)
	_, err = tr.Write(rPacket)
	if err != nil {
		r.Logger.Warnf("%v tr.Write: %v", err)
	}

	return err
}

func (r *Router) isPubKeyLocal(pk cipher.PubKey) bool {
	defer r.trFinish(r.trStart())

	for _, fct := range r.tm.Factories() {
		switch tfct := fct.(type) {
		case *transport.TCPFactory:
			if ipAddr := tfct.PkTable.RemoteAddr(pk); ipAddr != "" {
				return true
			}
		}
	}

	return false
}

func (r *Router) requestLoop(appConn *app.Protocol, raddr routing.Addr) (routing.Addr, error) {
	r.Logger.Info(th.Trace("ENTER"))
	defer r.Logger.Info(th.Trace("EXIT"))

	if r == nil {
		return raddr, nil
	}

	lport := r.pm.Alloc(appConn)
	if err := r.pm.SetLoop(lport, raddr, &loop{}); err != nil {
		r.Logger.Warnf("%v r.pm.SetLoop err: %v", err)
		return routing.Addr{}, err
	}

	laddr := routing.Addr{PubKey: r.config.PubKey, Port: lport}
	if raddr.PubKey == r.config.PubKey {
		if err := r.confirmLocalLoop(laddr, raddr); err != nil {
			r.Logger.Warnf("%v r.ConfirmLocalLoop err: %v", err)
			return routing.Addr{}, fmt.Errorf("confirm: %s", err)
		}
		r.trLog().Infof("r.confirmLocalLoop Created local loop on port %d", laddr.Port)
		return laddr, nil
	}

	forwardRoute, reverseRoute, err := r.fetchBestRoutes(laddr.PubKey, raddr.PubKey)
	if err != nil {
		r.Logger.Warnf("%v r.fetchBestRoutes err: %v\n", th.GetCaller(), err)
		return routing.Addr{}, fmt.Errorf("route finder: %s", err)
	}

	ld := routing.LoopDescriptor{
		Loop: routing.AddressPair{
			Local:  laddr,
			Remote: raddr,
		},
		Expiry:  time.Now().Add(RouteTTL),
		Forward: forwardRoute,
		Reverse: reverseRoute,
	}

	if r.isPubKeyLocal(raddr.PubKey) {
		r.Logger.Infof("%v laddr: %v raddr: %v \n", th.GetCaller(), laddr, raddr)
		if err := r.confirmTCPLoop(laddr, raddr); err != nil {
			r.Logger.Warnf("%v r.ConfirmTCPLoop err: %v", th.GetCaller(), err)

		}
		r.trLog().Infof("laddr: %v raddr: %v \n", th.GetCaller(), raddr)
		return laddr, err
	}

	proto, tr, err := r.setupProto(context.Background())
	if err != nil {
		r.Logger.Warnf("%v r.setupProto err: %v\n", err)
		return routing.Addr{}, err
	}

	defer func() {
		if err := tr.Close(); err != nil {
			r.Logger.Warnf("%v Failed to close transport: %s", err)
		}
	}()

	if err := setup.CreateLoop(proto, ld); err != nil {
		return routing.Addr{}, fmt.Errorf("route setup: %s", err)
	}

	r.Logger.Infof("%v success. Created new loop to %s on port %d", th.GetCaller(), raddr, laddr.Port)
	return laddr, nil
}

func (r *Router) confirmLocalLoop(laddr, raddr routing.Addr) error {
	r.Logger.Info(th.Trace("ENTER"))
	defer r.Logger.Info(th.Trace("EXIT"))

	b, err := r.pm.Get(raddr.Port)
	if err != nil {
		r.Logger.Infof("%v r.pm.Get %v", th.GetCaller(), err)
		return err
	}

	addrs := [2]routing.Addr{raddr, laddr}
	if err = b.conn.Send(app.FrameConfirmLoop, addrs, nil); err != nil {
		r.Logger.Warnf("%v b.conn.Send %v", th.GetCaller(), err)
		return err
	}

	return nil
}

func (r *Router) confirmTCPLoop(laddr, raddr routing.Addr) error {
	r.Logger.Info(th.Trace("ENTER"))
	defer r.Logger.Info(th.Trace("EXIT"))

	r.trLog().Infof("%v: laddr: %v, raddr: %v", th.GetCaller(), laddr, raddr)

	// portBind
	pb, err := r.pm.Get(raddr.Port)
	if err != nil {
		r.trLog().Warnf("%v r.pm.Get %v", th.GetCaller(), err)
		return err
	}

	addrs := [2]routing.Addr{raddr, laddr}
	if err = pb.conn.Send(app.FrameConfirmLoop, addrs, nil); err != nil {
		r.trLog().Warnf("%v b.conn.Send %v", th.GetCaller(), err)
		return err
	}

	tr, err := r.tm.CreateDataTransport(context.Background(), raddr.PubKey, "tcp-transport", true)
	if err != nil {
		r.trLog().Warnf("%v r.tm.CreateDataTransport err: %v\n", th.GetCaller(), err)
	}
	r.trLog().Infof("r.tm.CreateDataTransport  tr: %T  success: %v\n", tr, err == nil)

	return nil

}

func (r *Router) confirmLoop(l routing.AddressPair, rule routing.Rule) error {
	r.Logger.Info(th.Trace("ENTER"))
	defer r.Logger.Info(th.Trace("EXIT"))

	b, err := r.pm.Get(l.Local.Port)
	if err != nil {
		return err
	}

	if err := r.pm.SetLoop(l.Local.Port, l.Remote, &loop{rule.TransportID(), rule.RouteID()}); err != nil {
		return err
	}

	addrs := [2]routing.Addr{{PubKey: r.config.PubKey, Port: l.Local.Port}, l.Remote}
	if err = b.conn.Send(app.FrameConfirmLoop, addrs, nil); err != nil {
		r.Logger.Warnf("Failed to notify App about new loop: %s", err)
	}

	return nil
}

func (r *Router) closeLoop(loop routing.AddressPair) error {
	r.Logger.Info(th.Trace("ENTER"))
	defer r.Logger.Info(th.Trace("EXIT"))

	if err := r.destroyLoop(loop); err != nil {
		r.Logger.Warnf("Failed to remove loop: %s", err)
	}

	if r.isPubKeyLocal(loop.Remote.PubKey) {
		r.Logger.Infof("%v isPubKeyLocal() == true", th.GetCaller())
		return nil
	}
	proto, tr, err := r.setupProto(context.Background())
	if err != nil {
		return err
	}
	defer func() {
		if err := tr.Close(); err != nil {
			r.Logger.Warnf("Failed to close transport: %s", err)
		}
	}()

	ld := routing.LoopData{Loop: loop}
	if err := setup.CloseLoop(proto, ld); err != nil {
		return fmt.Errorf("route setup: %s", err)
	}

	r.Logger.Infof("Closed loop %s", loop)
	return nil
}

func (r *Router) loopClosed(loop routing.AddressPair) error {
	r.Logger.Info(th.Trace("ENTER"))
	defer r.Logger.Info(th.Trace("EXIT"))

	b, err := r.pm.Get(loop.Local.Port)
	if err != nil {
		return nil
	}

	if err := r.destroyLoop(loop); err != nil {
		r.Logger.Warnf("Failed to remove loop: %s", err)
	}

	if err := b.conn.Send(app.FrameClose, loop, nil); err != nil {
		return err
	}

	r.Logger.Infof("Closed loop %s", loop)
	return nil
}

func (r *Router) destroyLoop(loop routing.AddressPair) error {
	r.Logger.Info(th.Trace("ENTER"))
	defer r.Logger.Info(th.Trace("EXIT"))

	if r == nil {
		return nil
	}
	r.mu.Lock()
	_, ok := r.staticPorts[loop.Local.Port]
	r.mu.Unlock()

	if ok {
		if err := r.pm.RemoveLoop(loop.Local.Port, loop.Remote); err != nil {
			log.WithError(err).Warn("Failed to remove loop")
		}
	} else {
		r.pm.Close(loop.Local.Port)
	}

	return r.rm.RemoveLoopRule(loop)
}

func (r *Router) setupProto(ctx context.Context) (*setup.Protocol, transport.Transport, error) {
	r.Logger.Info(th.Trace("ENTER"))
	defer r.Logger.Info(th.Trace("EXIT"))

	if len(r.config.SetupNodes) == 0 {
		r.Logger.Warnf("%v : route setup: no nodes", th.GetCaller())
		return nil, nil, errors.New("route setup: no nodes")
	}

	tr, err := r.tm.CreateSetupTransport(ctx, r.config.SetupNodes[0], dmsg.Type)
	if err != nil {
		r.Logger.Warnf("%v  r.tm.CreateSetupTransport error:%v\n", th.GetCaller(), err)
		return nil, nil, fmt.Errorf("setup transport: %s", err)
	}

	sProto := setup.NewSetupProtocol(tr)
	return sProto, tr, nil
}

func (r *Router) fetchBestRoutes(source, destination cipher.PubKey) (fwd routing.Route, rev routing.Route, err error) {
	r.Logger.Info(th.Trace("ENTER"))
	defer r.Logger.Info(th.Trace("EXIT"))

	r.Logger.Infof("[Router.fetchBestRoutes] Requesting new routes from %s to %s", source, destination)
	timer := time.NewTimer(time.Second * 10)
	defer timer.Stop()

fetchRoutesAgain:
	fwdRoutes, revRoutes, err := r.config.RouteFinder.PairedRoutes(source, destination, minHops, maxHops)
	if err != nil {
		select {
		case <-timer.C:
			return nil, nil, err
		default:
			goto fetchRoutesAgain
		}
	}

	r.Logger.Infof("%v Found routes Forward: %s. Reverse %s success", th.GetCaller(), fwdRoutes, revRoutes)
	return fwdRoutes[0], revRoutes[0], nil
}

// IsSetupTransport checks whether `tr` is running in the `setup` mode.
func (r *Router) IsSetupTransport(tr *transport.ManagedTransport) bool {
	r.Logger.Info(th.Trace("ENTER"))
	defer r.Logger.Info(th.Trace("EXIT"))

	for _, pk := range r.config.SetupNodes {
		if tr.RemotePK() == pk {
			return true
		}
	}

	return false
}

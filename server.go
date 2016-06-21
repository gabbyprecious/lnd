package main

import (
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lndc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"

	"github.com/roasbeef/btcwallet/waddrmgr"
)

// server is the main server of the Lightning Network Daemon. The server
// houses global state pertianing to the wallet, database, and the rpcserver.
// Additionally, the server is also used as a central messaging bus to interact
// with any of its companion objects.
type server struct {
	started  int32 // atomic
	shutdown int32 // atomic

	// identityPriv is the private key used to authenticate any incoming
	// connections.
	identityPriv *btcec.PrivateKey

	listeners []net.Listener
	peers     map[int32]*peer

	chanIndexMtx sync.RWMutex
	chanIndex    map[wire.OutPoint]*peer

	rpcServer *rpcServer
	// TODO(roasbeef): add chan notifier also
	lnwallet *lnwallet.LightningWallet

	// TODO(roasbeef): add to constructor
	fundingMgr *fundingManager
	chanDB     *channeldb.DB

	newPeers  chan *peer
	donePeers chan *peer
	queries   chan interface{}

	wg   sync.WaitGroup
	quit chan struct{}
}

// newServer creates a new instance of the server which is to listen using the
// passed listener address.
func newServer(listenAddrs []string, wallet *lnwallet.LightningWallet,
	chanDB *channeldb.DB) (*server, error) {

	privKey, err := getIdentityPrivKey(chanDB, wallet)
	if err != nil {
		return nil, err
	}

	listeners := make([]net.Listener, len(listenAddrs))
	for i, addr := range listenAddrs {
		listeners[i], err = lndc.NewListener(privKey, addr)
		if err != nil {
			return nil, err
		}
	}

	s := &server{
		chanDB:       chanDB,
		fundingMgr:   newFundingManager(wallet),
		lnwallet:     wallet,
		identityPriv: privKey,
		listeners:    listeners,
		peers:        make(map[int32]*peer),
		chanIndex:    make(map[wire.OutPoint]*peer),
		newPeers:     make(chan *peer, 100),
		donePeers:    make(chan *peer, 100),
		queries:      make(chan interface{}),
		quit:         make(chan struct{}),
	}

	s.rpcServer = newRpcServer(s)

	return s, nil
}

// Start starts the main daemon server, all requested listeners, and any helper
// goroutines.
func (s *server) Start() {
	// Already running?
	if atomic.AddInt32(&s.started, 1) != 1 {
		return
	}

	// Start all the listeners.
	for _, l := range s.listeners {
		s.wg.Add(1)
		go s.listener(l)
	}

	s.fundingMgr.Start()

	s.wg.Add(2)
	go s.peerManager()
	go s.queryHandler()
}

// Stop gracefully shutsdown the main daemon server. This function will signal
// any active goroutines, or helper objects to exit, then blocks until they've
// all successfully exited. Additionally, any/all listeners are closed.
func (s *server) Stop() error {
	// Bail if we're already shutting down.
	if atomic.AddInt32(&s.shutdown, 1) != 1 {
		return nil
	}

	// Stop all the listeners.
	for _, listener := range s.listeners {
		if err := listener.Close(); err != nil {
			return err
		}
	}

	// Shutdown the wallet, funding manager, and the rpc server.
	s.rpcServer.Stop()
	s.lnwallet.Shutdown()
	s.fundingMgr.Stop()

	// Signal all the lingering goroutines to quit.
	close(s.quit)
	s.wg.Wait()

	return nil
}

// WaitForShutdown blocks all goroutines have been stopped.
func (s *server) WaitForShutdown() {
	s.wg.Wait()
}

// peerManager handles any requests to modify the server's internal state of
// all active peers. Additionally, any queries directed at peers will be
// handled by this goroutine.
//
// NOTE: This MUST be run as a goroutine.
func (s *server) peerManager() {
out:
	for {
		select {
		// New peers.
		case p := <-s.newPeers:
			s.addPeer(p)
		// Finished peers.
		case p := <-s.donePeers:
			s.removePeer(p)
		case <-s.quit:
			break out
		}
	}
	s.wg.Done()
}

// addPeer adds the passed peer to the server's global state of all active
// peers.
func (s *server) addPeer(p *peer) {
	if p == nil {
		return
	}

	// Ignore new peers if we're shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		p.Stop()
		return
	}

	s.peers[p.id] = p
}

// removePeer removes the passed peer from the server's state of all active
// peers.
func (s *server) removePeer(p *peer) {
	if p == nil {
		return
	}

	// Ignore deleting peers if we're shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		p.Stop()
		return
	}

	delete(s.peers, p.id)
}

// connectPeerMsg is a message requesting the server to open a connection to a
// particular peer. This message also houses an error channel which will be
// used to report success/failure.
type connectPeerMsg struct {
	addr *lndc.LNAdr
	resp chan int32
	err  chan error
}

// listPeersMsg is a message sent to the server in order to obtain a listing
// of all currently active channels.
type listPeersMsg struct {
	resp chan []*peer
}
// queryHandler is a a goroutine dedicated to handling an queries or requests
// to mutate the server's global state.
//
// NOTE: This MUST be run as a goroutine.
func (s *server) queryHandler() {
	// TODO(roabeef): consolidate with peerManager
out:
	for {
		select {
		case query := <-s.queries:
			// TODO(roasbeef): make all goroutines?
			switch msg := query.(type) {
			case *connectPeerMsg:
				s.handleConnectPeer(msg)
			case *listPeersMsg:
				s.handleListPeers(msg)
			}
		case <-s.quit:
			break out
		}
	}

	s.wg.Done()
}

// handleListPeers...
func (s *server) handleListPeers(msg *listPeersMsg) {
	peers := make([]*peer, 0, len(s.peers))
	for _, peer := range s.peers {
		peers = append(peers, peer)
	}

	msg.resp <- peers
}

// handleConnectPeer attempts to establish a connection to the address enclosed
// within the passed connectPeerMsg. This function is *async*, a goroutine will
// be spawned in order to finish the request, and respond to the caller.
func (s *server) handleConnectPeer(msg *connectPeerMsg) {
	addr := msg.addr

	// Ensure we're not already connected to this
	// peer.
	for _, peer := range s.peers {
		if peer.lightningAddr.String() ==
			addr.String() {
			msg.err <- fmt.Errorf(
				"already connected to peer: %v",
				peer.lightningAddr,
			)
			msg.resp <- -1
		}
	}

	// Launch a goroutine to connect to the requested
	// peer so we can continue to handle queries.
	// TODO(roasbeef): semaphore to limit the number of goroutines for
	// async requests.
	go func() {
		// For the lndc crypto handshake, we
		// either need a compressed pubkey, or a
		// 20-byte pkh.
		var remoteId []byte
		if addr.PubKey == nil {
			remoteId = addr.Base58Adr.ScriptAddress()
		} else {
			remoteId = addr.PubKey.SerializeCompressed()
		}

		srvrLog.Debugf("connecting to %v", hex.EncodeToString(remoteId))
		// Attempt to connect to the remote
		// node. If the we can't make the
		// connection, or the crypto negotation
		// breaks down, then return an error to the
		// caller.
		ipAddr := addr.NetAddr.String()
		conn := lndc.NewConn(nil)
		if err := conn.Dial(
			s.identityPriv, ipAddr, remoteId); err != nil {
			msg.err <- err
			msg.resp <- -1
			return
		}

		// Now that we've established a connection,
		// create a peer, and it to the set of
		// currently active peers.
		peer, err := newPeer(conn, s, activeNetParams.Net, false)
		if err != nil {
			srvrLog.Errorf("unable to create peer %v", err)
			msg.resp <- -1
			msg.err <- err
			return
		}

		peer.Start()
		s.newPeers <- peer

		msg.resp <- peer.id
		msg.err <- nil
	}()
}
	}

}

		return
	}

	}

}

	}

	for _, listener := range s.listeners {
	}


// Peers returns a slice of all active peers.
func (s *server) Peers() []*peer {
	resp := make(chan []*peer)

	s.queries <- &listPeersMsg{resp}

	return <-resp
}

// listener is a goroutine dedicated to accepting in coming peer connections
// from the passed listener.
//
// NOTE: This MUST be run as a goroutine.
func (s *server) listener(l net.Listener) {
	srvrLog.Infof("Server listening on %s", l.Addr())
	for atomic.LoadInt32(&s.shutdown) == 0 {
		conn, err := l.Accept()
		if err != nil {
			// Only log the error message if we aren't currently
			// shutting down.
			if atomic.LoadInt32(&s.shutdown) == 0 {
				srvrLog.Errorf("Can't accept connection: %v", err)
			}
			continue
		}

		srvrLog.Tracef("New inbound connection from %v", conn.RemoteAddr())
		peer, err := newPeer(conn, s, activeNetParams.Net, true)
		if err != nil {
			srvrLog.Errorf("unable to create peer: %v", err)
			continue
		}

		peer.Start()
		s.newPeers <- peer
	}

	s.wg.Done()
}

// getIdentityPrivKey gets the identity private key out of the wallet DB.
func getIdentityPrivKey(c *channeldb.DB,
	w *lnwallet.LightningWallet) (*btcec.PrivateKey, error) {

	// First retrieve the current identity address for this peer.
	adr, err := c.GetIdAdr()
	if err != nil {
		return nil, err
	}

	// Using the ID address, request the private key coresponding to the
	// address from the wallet's address manager.
	adr2, err := w.Manager.Address(adr)
	if err != nil {
		return nil, err
	}

	serializedKey := adr2.(waddrmgr.ManagedPubKeyAddress).PubKey().SerializeCompressed()
	keyEncoded := hex.EncodeToString(serializedKey)
	ltndLog.Infof("identity address: %v", adr)
	ltndLog.Infof("identity pubkey retrieved: %v", keyEncoded)

	priv, err := adr2.(waddrmgr.ManagedPubKeyAddress).PrivKey()
	if err != nil {
		return nil, err
	}

	return priv, nil
}

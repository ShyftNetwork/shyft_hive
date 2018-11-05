// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"container/list"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/ShyftNetwork/go-empyrean/crypto"
	"github.com/ShyftNetwork/go-empyrean/log"
	"github.com/ShyftNetwork/go-empyrean/p2p/enode"
	"github.com/ShyftNetwork/go-empyrean/p2p/nat"
	"github.com/ShyftNetwork/go-empyrean/p2p/netutil"
	"github.com/ShyftNetwork/go-empyrean/rlp"
)

// Errors
var (
	errPacketTooSmall   = errors.New("too small")
	errBadHash          = errors.New("bad hash")
	errExpired          = errors.New("expired")
	errUnsolicitedReply = errors.New("unsolicited reply")
	errUnknownNode      = errors.New("unknown node")
	errTimeout          = errors.New("RPC timeout")
	errClockWarp        = errors.New("reply deadline too far in the future")
	errClosed           = errors.New("socket closed")
	errResponseReceived = errors.New("response received")
	errPacketMismatch   = errors.New("packet mismatch")
	errCorruptDHT       = errors.New("corrupt neighbours data")
	unexpectedPacket    = false
)

// Timeouts
const (
	respTimeout    = 500 * time.Millisecond
	expiration     = 20 * time.Second
	bondExpiration = 24 * time.Hour

	ntpFailureThreshold = 32               // Continuous timeouts after which to check NTP
	ntpWarningCooldown  = 10 * time.Minute // Minimum amount of time to pass before repeating NTP warning
	driftThreshold      = 10 * time.Second // Allowed clock drift before warning user
)

// RPC packet types
const (
	pingPacket = iota + 1 // zero is 'reserved'
	pongPacket
	findnodePacket
	neighborsPacket
	garbagePacket1
	garbagePacket2
	garbagePacket3
	garbagePacket4
	garbagePacket5
	garbagePacket6
	garbagePacket7
	garbagePacket8
)

// RPC request structures
type (
	ping struct {
		Version    uint
		From, To   rpcEndpoint
		Expiration uint64
		// Ignore additional fields (for forward compatibility).
		Rest []rlp.RawValue `rlp:"tail"`
	}

	pingExtra struct {
		Version    uint
		From, To   rpcEndpoint
		Expiration uint64
		JunkData1  uint
		JunkData2  []byte
		// Ignore additional fields (for forward compatibility).
		Rest []rlp.RawValue `rlp:"tail"`
	}

	// pong is the reply to ping.
	pong struct {
		// This field should mirror the UDP envelope address
		// of the ping packet, which provides a way to discover the
		// the external address (after NAT).
		To rpcEndpoint

		ReplyTok   []byte // This contains the hash of the ping packet.
		Expiration uint64 // Absolute timestamp at which the packet becomes invalid.
		// Ignore additional fields (for forward compatibility).
		Rest []rlp.RawValue `rlp:"tail"`
	}

	// findnode is a query for nodes close to the given target.
	findnode struct {
		Target     encPubkey
		Expiration uint64
		// Ignore additional fields (for forward compatibility).
		Rest []rlp.RawValue `rlp:"tail"`
	}

	// reply to findnode
	neighbors struct {
		Nodes      []rpcNode
		Expiration uint64
		// Ignore additional fields (for forward compatibility).
		Rest []rlp.RawValue `rlp:"tail"`
	}

	incomingPacket struct {
		packet      interface{}
		recoveredID encPubkey
	}

	rpcNode struct {
		IP  net.IP // len 4 for IPv4 or 16 for IPv6
		UDP uint16 // for discovery protocol
		TCP uint16 // for RLPx protocol
		ID  encPubkey
	}

	rpcEndpoint struct {
		IP  net.IP // len 4 for IPv4 or 16 for IPv6
		UDP uint16 // for discovery protocol
		TCP uint16 // for RLPx protocol
	}
)

func makeEndpoint(addr *net.UDPAddr, tcpPort uint16) rpcEndpoint {
	ip := addr.IP.To4()
	if ip == nil {
		ip = addr.IP.To16()
	}
	return rpcEndpoint{IP: ip, UDP: uint16(addr.Port), TCP: tcpPort}
}

func (t *V4Udp) nodeFromRPC(sender *net.UDPAddr, rn rpcNode) (*node, error) {
	if rn.UDP <= 1024 {
		return nil, errors.New("low port")
	}
	if err := netutil.CheckRelayIP(sender.IP, rn.IP); err != nil {
		return nil, err
	}
	if t.netrestrict != nil && !t.netrestrict.Contains(rn.IP) {
		return nil, errors.New("not contained in netrestrict whitelist")
	}
	key, err := decodePubkey(rn.ID)
	if err != nil {
		return nil, err
	}
	n := wrapNode(enode.NewV4(key, rn.IP, int(rn.TCP), int(rn.UDP)))
	err = n.ValidateComplete()
	return n, err
}

func nodeToRPC(n *node) rpcNode {
	var key ecdsa.PublicKey
	var ekey encPubkey
	if err := n.Load((*enode.Secp256k1)(&key)); err == nil {
		ekey = encodePubkey(&key)
	}
	return rpcNode{ID: ekey, IP: n.IP(), UDP: uint16(n.UDP()), TCP: uint16(n.TCP())}
}

type packet interface {
	handle(t *V4Udp, from *net.UDPAddr, fromKey encPubkey, mac []byte) error
	name() string
}

type conn interface {
	ReadFromUDP(b []byte) (n int, addr *net.UDPAddr, err error)
	WriteToUDP(b []byte, addr *net.UDPAddr) (n int, err error)
	Close() error
	LocalAddr() net.Addr
}

//V4Udp is the v4UDP test class
type V4Udp struct {
	conn        conn
	netrestrict *netutil.Netlist
	priv        *ecdsa.PrivateKey
	ourEndpoint rpcEndpoint

	addpending chan *pending
	gotreply   chan reply

	closing chan struct{}
	nat     nat.Interface
}

// pending represents a pending reply.
//
// some implementations of the protocol wish to send more than one
// reply packet to findnode. in general, any neighbors packet cannot
// be matched up with a specific findnode packet.
//
// our implementation handles this by storing a callback function for
// each pending reply. incoming packets from a node are dispatched
// to all the callback functions for that node.
type pending struct {
	// these fields must match in the reply.
	from enode.ID

	// time when the request must complete
	deadline time.Time

	//callback is called when a packet is received. if it returns nil,
	//the callback is removed from the pending reply queue (handled successfully and expected by test case).
	//if it returns a mismatch error, (ignored by callback, further 'pendings' may be in the test case)
	//if it returns any other error, that error is considered the outcome of the
	//'pending' operation

	//callback func(resp interface{}) (done error)
	callback func(resp reply) (done error)

	// errc receives nil when the callback indicates completion or an
	// error if no further reply is received within the timeout.
	errc chan<- error
}

type reply struct {
	from  enode.ID
	ptype byte
	data  interface{}
	// loop indicates whether there was
	// a matching request by sending on this channel.
	matched chan<- bool
}

// ReadPacket is sent to the unhandled channel when it could not be processed
type ReadPacket struct {
	Data []byte
	Addr *net.UDPAddr
}

// Config holds Table-related settings.
type Config struct {
	// These settings are required and configure the UDP listener:
	PrivateKey *ecdsa.PrivateKey

	// These settings are optional:
	AnnounceAddr *net.UDPAddr      // local address announced in the DHT
	NodeDBPath   string            // if set, the node database is stored at this filesystem location
	NetRestrict  *netutil.Netlist  // network whitelist
	Bootnodes    []*enode.Node     // list of bootstrap nodes
	Unhandled    chan<- ReadPacket // unhandled packets are sent on this channel
}

// ListenUDP returns a new table that listens for UDP packets on laddr.
func ListenUDP(c conn, cfg Config) (*V4Udp, error) {
	v4Udp, err := newUDP(c, cfg)
	if err != nil {
		return nil, err
	}
	log.Info("UDP listener up", "self")
	return v4Udp, nil
}

func newUDP(c conn, cfg Config) (*V4Udp, error) {
	realaddr := c.LocalAddr().(*net.UDPAddr)
	if cfg.AnnounceAddr != nil {
		realaddr = cfg.AnnounceAddr
	}
	//	self := enode.NewV4(&cfg.PrivateKey.PublicKey, realaddr.IP, realaddr.Port, realaddr.Port)
	//	db, err := enode.OpenDB(cfg.NodeDBPath)
	if err != nil {
		return nil, err
	}

	udp := &V4Udp{
		conn:        c,
		priv:        cfg.PrivateKey,
		netrestrict: cfg.NetRestrict,
		closing:     make(chan struct{}),
		gotreply:    make(chan reply),
		addpending:  make(chan *pending),
	}

	udp.ourEndpoint = makeEndpoint(realaddr, uint16(realaddr.Port))
	//	tab, err := newTable(udp, self, db, cfg.Bootnodes)
	if err != nil {
		return nil, err
	}
	//	udp.Table = tab

	go udp.loop()
	go udp.readLoop(cfg.Unhandled)
	return udp, nil
}

func (t *V4Udp) close() {
	close(t.closing)
	t.conn.Close()
	//t.db.Close()

}

// ping sends a ping message to the given node and waits for a reply.
func (t *V4Udp) ping(toid enode.ID, toaddr *net.UDPAddr, validateEnodeID bool, recoveryCallback func(e *ecdsa.PublicKey)) error {

	to := makeEndpoint(toaddr, 0)

	req := &ping{
		Version:    4,
		From:       t.ourEndpoint,
		To:         to, // TODO: maybe use known TCP port from DB
		Expiration: uint64(time.Now().Add(expiration).Unix()),
	}

	packet, hash, err := encodePacket(t.priv, pingPacket, req)
	if err != nil {
		return err
	}

	callback := func(p reply) error {
		if p.ptype == pongPacket {
			inPacket := p.data.(incomingPacket)

			if !bytes.Equal(inPacket.packet.(*pong).ReplyTok, hash) {
				return errUnsolicitedReply
			}

			if validateEnodeID && toid != inPacket.recoveredID.id() {
				return errUnknownNode
			}

			if recoveryCallback != nil {
				key, err := decodePubkey(inPacket.recoveredID)
				if err != nil {
					recoveryCallback(key)
				}
			}
		} else {
			return errPacketMismatch
		}
		return nil

	}
	return <-t.sendPacket(toid, toaddr, req, packet, callback)

}

func (t *V4Udp) pingWrongFrom(toid enode.ID, toaddr *net.UDPAddr, validateEnodeID bool, recoveryCallback func(e *ecdsa.PublicKey)) error {

	to := makeEndpoint(toaddr, 0)

	from := makeEndpoint(&net.UDPAddr{IP: []byte{0, 1, 2, 3}, Port: 1}, 0) //this is a garbage endpoint

	req := &ping{
		Version:    4,
		From:       from,
		To:         to, // TODO: maybe use known TCP port from DB
		Expiration: uint64(time.Now().Add(expiration).Unix()),
	}

	packet, hash, err := encodePacket(t.priv, pingPacket, req)
	if err != nil {
		return err
	}

	//expect the usual ping stuff - a bad 'from' should be ignored
	callback := func(p reply) error {
		if p.ptype == pongPacket {
			inPacket := p.data.(incomingPacket)

			if !bytes.Equal(inPacket.packet.(*pong).ReplyTok, hash) {
				return errUnsolicitedReply
			}

			if validateEnodeID && toid != inPacket.recoveredID.id() {
				return errUnknownNode
			}

			if recoveryCallback != nil {
				key, err := decodePubkey(inPacket.recoveredID)
				if err != nil {
					recoveryCallback(key)
				}
			}
		} else {
			return errPacketMismatch
		}
		return nil

	}
	return <-t.sendPacket(toid, toaddr, req, packet, callback)

}

func (t *V4Udp) pingWrongTo(toid enode.ID, toaddr *net.UDPAddr, validateEnodeID bool, recoveryCallback func(e *ecdsa.PublicKey)) error {

	to := makeEndpoint(&net.UDPAddr{IP: []byte{0, 1, 2, 3}, Port: 1}, 0)

	req := &ping{
		Version:    4,
		From:       t.ourEndpoint,
		To:         to, // TODO: maybe use known TCP port from DB
		Expiration: uint64(time.Now().Add(expiration).Unix()),
	}

	packet, _, err := encodePacket(t.priv, pingPacket, req)
	if err != nil {
		return err
	}

	callback := func(p reply) error {
		if p.ptype == pongPacket {
			return nil
		}

		return errPacketMismatch
	}
	return <-t.sendPacket(toid, toaddr, req, packet, callback)

}

//ping with a 'future format' packet containing extra fields
func (t *V4Udp) pingExtraData(toid enode.ID, toaddr *net.UDPAddr, validateEnodeID bool, recoveryCallback func(e *ecdsa.PublicKey)) error {

	to := makeEndpoint(toaddr, 0)

	req := &pingExtra{
		Version:   4,
		From:      t.ourEndpoint,
		To:        to,
		JunkData1: 42,
		JunkData2: []byte{9, 8, 7, 6, 5, 4, 3, 2, 1},

		Expiration: uint64(time.Now().Add(expiration).Unix()),
	}

	packet, hash, err := encodePacket(t.priv, pingPacket, req)
	if err != nil {
		return err
	}

	//expect the usual ping responses
	callback := func(p reply) error {
		if p.ptype == pongPacket {
			inPacket := p.data.(incomingPacket)

			if !bytes.Equal(inPacket.packet.(*pong).ReplyTok, hash) {
				return errUnsolicitedReply
			}

			if validateEnodeID && toid != inPacket.recoveredID.id() {
				return errUnknownNode
			}

			if recoveryCallback != nil {
				key, err := decodePubkey(inPacket.recoveredID)
				if err != nil {
					recoveryCallback(key)
				}
			}
		} else {
			return errPacketMismatch
		}
		return nil
	}
	return <-t.sendPacket(toid, toaddr, &ping{}, packet, callback) //the dummy ping is just to get the name

}

//ping with a 'future format' packet containing extra fields and make sure it works even with the wrong 'from' field
func (t *V4Udp) pingExtraDataWrongFrom(toid enode.ID, toaddr *net.UDPAddr, validateEnodeID bool, recoveryCallback func(e *ecdsa.PublicKey)) error {

	to := makeEndpoint(toaddr, 0)

	from := makeEndpoint(&net.UDPAddr{IP: []byte{0, 1, 2, 3}, Port: 1}, 0) //this is a garbage endpoint

	req := &pingExtra{
		Version:   4,
		From:      from,
		To:        to,
		JunkData1: 42,
		JunkData2: []byte{9, 8, 7, 6, 5, 4, 3, 2, 1},

		Expiration: uint64(time.Now().Add(expiration).Unix()),
	}

	packet, hash, err := encodePacket(t.priv, pingPacket, req)
	if err != nil {
		return err
	}

	//expect the usual ping reponses
	callback := func(p reply) error {
		if p.ptype == pongPacket {
			inPacket := p.data.(incomingPacket)

			if !bytes.Equal(inPacket.packet.(*pong).ReplyTok, hash) {
				return errUnsolicitedReply
			}

			if validateEnodeID && toid != inPacket.recoveredID.id() {
				return errUnknownNode
			}

			if recoveryCallback != nil {
				key, err := decodePubkey(inPacket.recoveredID)
				if err != nil {
					recoveryCallback(key)
				}
			}
		} else {
			return errPacketMismatch
		}
		return nil
	}
	return <-t.sendPacket(toid, toaddr, &ping{}, packet, callback) //the dummy ping is just to get the name

}

// send a packet (a ping packet, though it could be something else) with an unknown packet type to the client and
// see how the target behaves. If the target responds to the ping, then fail.
func (t *V4Udp) pingTargetWrongPacketType(toid enode.ID, toaddr *net.UDPAddr, validateEnodeID bool, recoveryCallback func(e *ecdsa.PublicKey)) error {

	to := makeEndpoint(toaddr, 0)

	req := &ping{
		Version:    4,
		From:       t.ourEndpoint,
		To:         to, // TODO: maybe use known TCP port from DB
		Expiration: uint64(time.Now().Add(expiration).Unix()),
	}

	packet, _, err := encodePacket(t.priv, garbagePacket8, req)
	if err != nil {
		return err
	}

	//expect anything but a ping or pong
	callback := func(p reply) error {
		if p.ptype == pongPacket {
			return errUnsolicitedReply
		}

		if p.ptype == pingPacket {
			return errUnsolicitedReply
		}

		return errPacketMismatch
	}
	return <-t.sendPacket(toid, toaddr, req, packet, callback)

}

func (t *V4Udp) findnodeWithoutBond(toid enode.ID, toaddr *net.UDPAddr, target encPubkey) error {

	req := &findnode{
		Target:     target,
		Expiration: uint64(time.Now().Add(expiration).Unix()),
	}

	packet, _, err := encodePacket(t.priv, findnodePacket, req)
	if err != nil {
		return err
	}

	//expect nothing
	callback := func(p reply) error {

		return errUnsolicitedReply
	}

	return <-t.sendPacket(toid, toaddr, req, packet, callback)

}

func (t *V4Udp) pingBondedWithMangledFromField(toid enode.ID, toaddr *net.UDPAddr, validateEnodeID bool, recoveryCallback func(e *ecdsa.PublicKey)) error {

	//try to bond with the target using normal ping data
	err = t.ping(toid, toaddr, false, nil)
	if err != nil {
		return err
	}
	//hang around for a bit (we don't know if the target was already bonded or not)
	time.Sleep(2 * time.Second)

	to := makeEndpoint(toaddr, 0)

	from := makeEndpoint(&net.UDPAddr{IP: []byte{0, 1, 2, 3}, Port: 1}, 0) //this is a garbage endpoint

	req := &ping{
		Version:    4,
		From:       from,
		To:         to, // TODO: maybe use known TCP port from DB
		Expiration: uint64(time.Now().Add(expiration).Unix()),
	}

	packet, hash, err := encodePacket(t.priv, pingPacket, req)
	if err != nil {
		return err
	}

	//expect the usual ping stuff - a bad 'from' should be ignored
	callback := func(p reply) error {
		if p.ptype == pongPacket {
			inPacket := p.data.(incomingPacket)

			if !bytes.Equal(inPacket.packet.(*pong).ReplyTok, hash) {
				return errUnsolicitedReply
			}

			if validateEnodeID && toid != inPacket.recoveredID.id() {
				return errUnknownNode
			}

			if recoveryCallback != nil {
				key, err := decodePubkey(inPacket.recoveredID)
				if err != nil {
					recoveryCallback(key)
				}
			}
		} else {
			return errPacketMismatch
		}
		return nil

	}
	return <-t.sendPacket(toid, toaddr, req, packet, callback)

}

func (t *V4Udp) bondedSourceFindNeighbours(toid enode.ID, toaddr *net.UDPAddr, target encPubkey) error {
	//try to bond with the target
	err = t.ping(toid, toaddr, false, nil)
	if err != nil {
		return err
	}
	//hang around for a bit (we don't know if the target was already bonded or not)
	time.Sleep(2 * time.Second)

	//send an unsolicited neighbours packet
	req := neighbors{Expiration: uint64(time.Now().Add(expiration).Unix())}
	var fakeKey *ecdsa.PrivateKey
	if fakeKey, err = crypto.GenerateKey(); err != nil {
		return err
	}
	fakePub := fakeKey.PublicKey
	encFakeKey := encodePubkey(&fakePub)
	fakeNeighbour := rpcNode{ID: encFakeKey, IP: net.IP{1, 2, 3, 4}, UDP: 123, TCP: 123}
	req.Nodes = []rpcNode{fakeNeighbour}

	t.send(toaddr, neighborsPacket, &req)

	//now call find neighbours
	findReq := &findnode{
		Target:     target,
		Expiration: uint64(time.Now().Add(expiration).Unix()),
	}

	packet, _, err := encodePacket(t.priv, findnodePacket, findReq)
	if err != nil {
		return err
	}

	//expect good neighbours response with no junk
	callback := func(p reply) error {

		if p.ptype == neighborsPacket {
			//got a response.
			//we assume the target is not connected to a public or populated bootnode
			//so we assume the target does not have any other neighbours in the DHT
			inPacket := p.data.(incomingPacket)

			for _, neighbour := range inPacket.packet.(*neighbors).Nodes {
				if neighbour.ID == encFakeKey {
					return errCorruptDHT
				}
			}
			return nil

		}
		return errUnsolicitedReply
	}

	return <-t.sendPacket(toid, toaddr, findReq, packet, callback)

}

// ping sends a ping message to the given node and waits for a reply.
func (t *V4Udp) pingPastExpiration(toid enode.ID, toaddr *net.UDPAddr, validateEnodeID bool, recoveryCallback func(e *ecdsa.PublicKey)) error {

	to := makeEndpoint(toaddr, 0)

	req := &ping{
		Version:    4,
		From:       t.ourEndpoint,
		To:         to, // TODO: maybe use known TCP port from DB
		Expiration: uint64(time.Now().Add(-expiration).Unix()),
	}

	packet, _, err := encodePacket(t.priv, pingPacket, req)
	if err != nil {
		return err
	}

	//expect no pong
	callback := func(p reply) error {
		if p.ptype == pongPacket {
			return errUnsolicitedReply
		}
		return errPacketMismatch

	}
	return <-t.sendPacket(toid, toaddr, req, packet, callback)

}

func (t *V4Udp) bondedSourceFindNeighboursPastExpiration(toid enode.ID, toaddr *net.UDPAddr, target encPubkey) error {
	//try to bond with the target
	err = t.ping(toid, toaddr, false, nil)
	if err != nil {
		return err
	}
	//hang around for a bit (we don't know if the target was already bonded or not)
	time.Sleep(2 * time.Second)

	//now call find neighbours
	findReq := &findnode{
		Target:     target,
		Expiration: uint64(time.Now().Add(-expiration).Unix()),
	}

	packet, _, err := encodePacket(t.priv, findnodePacket, findReq)
	if err != nil {
		return err
	}

	//expect good neighbours response with no junk
	callback := func(p reply) error {

		if p.ptype == neighborsPacket {
			return errUnsolicitedReply

		}
		return errPacketMismatch
	}

	return <-t.sendPacket(toid, toaddr, findReq, packet, callback)

}

func (t *V4Udp) sendPacket(toid enode.ID, toaddr *net.UDPAddr, req packet, packet []byte, callback func(reply) error) <-chan error {

	errc := t.pending(toid, callback)
	t.write(toaddr, req.name(), packet)
	return errc
}

// func (t *V4Udp) waitping(from enode.ID) error {
// 	return <-t.pending(from, pingPacket, func(interface{}) bool { return true })
// }

// findnode sends a findnode request to the given node and waits until
// the node has sent up to k neighbors.
//func (t *V4Udp) findnode(toid enode.ID, toaddr *net.UDPAddr, target encPubkey) ([]*node, error) {

// If we haven't seen a ping from the destination node for a while, it won't remember
// our endpoint proof and reject findnode. Solicit a ping first.

//!!!!!*******TODO *******!!!!!!
//Replace this with a test-scoped variable
//!!!************************!!!
// if time.Since(t.db.LastPingReceived(toid)) > bondExpiration {
// 	t.ping(toid, toaddr)
// 	t.waitping(toid)
// }
//bucketSize

//*********************//
// bucketSize := 16
// nodes := make([]*node, 0, bucketSize)
// nreceived := 0
// errc := t.pending(toid, neighborsPacket, func(r interface{}) bool {
// 	reply := r.(incomingPacket).packet.(*neighbors)
// 	for _, rn := range reply.Nodes {
// 		nreceived++
// 		n, err := t.nodeFromRPC(toaddr, rn)
// 		if err != nil {
// 			log.Trace("Invalid neighbor node received", "ip", rn.IP, "addr", toaddr, "err", err)
// 			continue
// 		}
// 		nodes = append(nodes, n)
// 	}
// 	return nreceived >= bucketSize
// })
// t.send(toaddr, findnodePacket, &findnode{
// 	Target:     target,
// 	Expiration: uint64(time.Now().Add(expiration).Unix()),
// })
//return nodes, <-errc
//return nil, nil
//}

// pending adds a reply callback to the pending reply queue.
// see the documentation of type pending for a detailed explanation.
func (t *V4Udp) pending(id enode.ID, callback func(reply) error) <-chan error {
	ch := make(chan error, 1)
	p := &pending{from: id, callback: callback, errc: ch}
	select {
	case t.addpending <- p:
		// loop will handle it
	case <-t.closing:
		ch <- errClosed
	}
	return ch
}

func (t *V4Udp) handleReply(from enode.ID, ptype byte, req incomingPacket) bool {
	matched := make(chan bool, 1)
	select {
	case t.gotreply <- reply{from, ptype, req, matched}:
		// loop will handle it
		return <-matched
	case <-t.closing:
		return false
	}
}

// loop runs in its own goroutine. it keeps track of
// the refresh timer and the pending reply queue.
func (t *V4Udp) loop() {
	var (
		plist        = list.New()
		timeout      = time.NewTimer(0)
		nextTimeout  *pending // head of plist when timeout was last reset
		contTimeouts = 0      // number of continuous timeouts to do NTP checks
	//	ntpWarnTime  = time.Unix(0, 0)
	)
	<-timeout.C // ignore first timeout
	defer timeout.Stop()

	resetTimeout := func() {
		if plist.Front() == nil || nextTimeout == plist.Front().Value {
			return
		}
		// Start the timer so it fires when the next pending reply has expired.
		now := time.Now()
		for el := plist.Front(); el != nil; el = el.Next() {
			nextTimeout = el.Value.(*pending)
			if dist := nextTimeout.deadline.Sub(now); dist < 2*respTimeout {
				timeout.Reset(dist)
				return
			}
			// Remove pending replies whose deadline is too far in the
			// future. These can occur if the system clock jumped
			// backwards after the deadline was assigned.
			nextTimeout.errc <- errClockWarp
			plist.Remove(el)
		}
		nextTimeout = nil
		timeout.Stop()
	}

	for {
		resetTimeout()

		select {
		case <-t.closing:
			for el := plist.Front(); el != nil; el = el.Next() {
				el.Value.(*pending).errc <- errClosed
			}
			return

		case p := <-t.addpending:
			p.deadline = time.Now().Add(respTimeout)
			plist.PushBack(p)

		case r := <-t.gotreply:
			var matched bool
			for el := plist.Front(); el != nil; el = el.Next() {
				p := el.Value.(*pending)
				if p.from == r.from {

					// Remove the matcher if its callback indicates
					// that all replies have been received. This is
					// required for packet types that expect multiple
					// reply packets.

					cbres := p.callback(r)
					if cbres != errPacketMismatch {
						matched = true
						if cbres == nil {
							plist.Remove(el)
							p.errc <- nil
						} else {
							plist.Remove(el)
							p.errc <- cbres
						}
					}

					// Reset the continuous timeout counter (time drift detection)
					contTimeouts = 0
				}
			}
			r.matched <- matched

		case now := <-timeout.C:
			nextTimeout = nil

			// Notify and remove callbacks whose deadline is in the past.
			for el := plist.Front(); el != nil; el = el.Next() {
				p := el.Value.(*pending)
				if now.After(p.deadline) || now.Equal(p.deadline) {
					p.errc <- errTimeout
					plist.Remove(el)
					contTimeouts++
				}
			}
			// If we've accumulated too many timeouts, do an NTP time sync check

			//****************************************
			//TODO: Replace with something under test
			//control
			//****************************************

			// if contTimeouts > ntpFailureThreshold {
			// 	if time.Since(ntpWarnTime) >= ntpWarningCooldown {
			// 		ntpWarnTime = time.Now()
			// 		go checkClockDrift()
			// 	}
			// 	contTimeouts = 0
			// }
		}
	}
}

const (
	macSize  = 256 / 8
	sigSize  = 520 / 8
	headSize = macSize + sigSize // space of packet frame data
)

var (
	headSpace = make([]byte, headSize)

	// Neighbors replies are sent across multiple packets to
	// stay below the 1280 byte limit. We compute the maximum number
	// of entries by stuffing a packet until it grows too large.
	maxNeighbors int
)

func init() {
	p := neighbors{Expiration: ^uint64(0)}
	maxSizeNode := rpcNode{IP: make(net.IP, 16), UDP: ^uint16(0), TCP: ^uint16(0)}
	for n := 0; ; n++ {
		p.Nodes = append(p.Nodes, maxSizeNode)
		size, _, err := rlp.EncodeToReader(p)
		if err != nil {
			// If this ever happens, it will be caught by the unit tests.
			panic("cannot encode: " + err.Error())
		}
		if headSize+size+1 >= 1280 {
			maxNeighbors = n
			break
		}
	}
}

func (t *V4Udp) send(toaddr *net.UDPAddr, ptype byte, req packet) ([]byte, error) {
	packet, hash, err := encodePacket(t.priv, ptype, req)
	if err != nil {
		return hash, err
	}
	return hash, t.write(toaddr, req.name(), packet)
}

func (t *V4Udp) write(toaddr *net.UDPAddr, what string, packet []byte) error {
	_, err := t.conn.WriteToUDP(packet, toaddr)
	log.Trace(">> "+what, "addr", toaddr, "err", err)
	return err
}

func encodePacket(priv *ecdsa.PrivateKey, ptype byte, req interface{}) (packet, hash []byte, err error) {
	b := new(bytes.Buffer)
	b.Write(headSpace)
	b.WriteByte(ptype)
	if err := rlp.Encode(b, req); err != nil {
		log.Error("Can't encode discv4 packet", "err", err)
		return nil, nil, err
	}
	packet = b.Bytes()
	sig, err := crypto.Sign(crypto.Keccak256(packet[headSize:]), priv)
	if err != nil {
		log.Error("Can't sign discv4 packet", "err", err)
		return nil, nil, err
	}
	copy(packet[macSize:], sig)
	// add the hash to the front. Note: this doesn't protect the
	// packet in any way. Our public key will be part of this hash in
	// The future.
	hash = crypto.Keccak256(packet[macSize:])
	copy(packet, hash)
	return packet, hash, nil
}

// readLoop runs in its own goroutine. it handles incoming UDP packets.
func (t *V4Udp) readLoop(unhandled chan<- ReadPacket) {
	defer t.conn.Close()
	if unhandled != nil {
		defer close(unhandled)
	}
	// Discovery packets are defined to be no larger than 1280 bytes.
	// Packets larger than this size will be cut at the end and treated
	// as invalid because their hash won't match.
	buf := make([]byte, 1280)
	for {
		nbytes, from, err := t.conn.ReadFromUDP(buf)
		if netutil.IsTemporaryError(err) {
			// Ignore temporary read errors.
			log.Debug("Temporary UDP read error", "err", err)
			continue
		} else if err != nil {
			// Shut down the loop for permament errors.
			log.Debug("UDP read error", "err", err)
			return
		}
		if t.handlePacket(from, buf[:nbytes]) != nil && unhandled != nil {
			select {
			case unhandled <- ReadPacket{buf[:nbytes], from}:
			default:
			}
		}
	}
}

func (t *V4Udp) handlePacket(from *net.UDPAddr, buf []byte) error {
	inpacket, fromKey, hash, err := decodePacket(buf)
	if err != nil {
		log.Debug("Bad discv4 packet", "addr", from, "err", err)
		return err
	}
	err = inpacket.handle(t, from, fromKey, hash)
	log.Trace("<< "+inpacket.name(), "addr", from, "err", err)
	return err
}

func decodePacket(buf []byte) (packet, encPubkey, []byte, error) {

	if len(buf) < headSize+1 {
		return nil, encPubkey{}, nil, errPacketTooSmall
	}
	hash, sig, sigdata := buf[:macSize], buf[macSize:headSize], buf[headSize:]
	shouldhash := crypto.Keccak256(buf[macSize:])
	if !bytes.Equal(hash, shouldhash) {
		return nil, encPubkey{}, nil, errBadHash
	}
	fromKey, err := recoverNodeKey(crypto.Keccak256(buf[headSize:]), sig)
	if err != nil {
		return nil, fromKey, hash, err
	}

	var req packet
	switch ptype := sigdata[0]; ptype {
	case pingPacket:
		req = new(ping)
	case pongPacket:
		req = new(pong)
	case findnodePacket:
		req = new(findnode)
	case neighborsPacket:
		req = new(neighbors)
	default:
		return req, fromKey, hash, fmt.Errorf("unknown type: %d", ptype)
	}
	s := rlp.NewStream(bytes.NewReader(sigdata[1:]), 0)
	err = s.Decode(req)

	return req, fromKey, hash, err
}

func (req *ping) handle(t *V4Udp, from *net.UDPAddr, fromKey encPubkey, mac []byte) error {
	if expired(req.Expiration) {
		return errExpired
	}
	key, err := decodePubkey(fromKey)
	if err != nil {
		return fmt.Errorf("invalid public key: %v", err)
	}
	t.send(from, pongPacket, &pong{
		To:         makeEndpoint(from, req.From.TCP),
		ReplyTok:   mac,
		Expiration: uint64(time.Now().Add(expiration).Unix()),
	})
	n := wrapNode(enode.NewV4(key, from.IP, int(req.From.TCP), from.Port))
	t.handleReply(n.ID(), pingPacket, incomingPacket{packet: req, recoveredID: fromKey})

	return nil
}

func (req *ping) name() string { return "PING/v4" }

func (req *pong) handle(t *V4Udp, from *net.UDPAddr, fromKey encPubkey, mac []byte) error {
	if expired(req.Expiration) {
		return errExpired
	}
	fromID := fromKey.id()
	t.handleReply(fromID, pongPacket, incomingPacket{packet: req, recoveredID: fromKey})

	return nil
}

func (req *pong) name() string { return "PONG/v4" }

func (req *findnode) handle(t *V4Udp, from *net.UDPAddr, fromKey encPubkey, mac []byte) error {
	if expired(req.Expiration) {
		return errExpired
	}
	//********************************
	//TODO
	//********************************
	//fromID := fromKey.id()

	//if time.Since(t.db.LastPongReceived(fromID)) > bondExpiration {
	// No endpoint proof pong exists, we don't process the packet. This prevents an
	// attack vector where the discovery protocol could be used to amplify traffic in a
	// DDOS attack. A malicious actor would send a findnode request with the IP address
	// and UDP port of the target as the source address. The recipient of the findnode
	// packet would then send a neighbors packet (which is a much bigger packet than
	// findnode) to the victim.
	//	return errUnknownNode
	//}
	// target := enode.ID(crypto.Keccak256Hash(req.Target[:]))
	// t.mutex.Lock()
	// closest := t.closest(target, bucketSize).entries
	// t.mutex.Unlock()

	// p := neighbors{Expiration: uint64(time.Now().Add(expiration).Unix())}
	// var sent bool
	// // Send neighbors in chunks with at most maxNeighbors per packet
	// // to stay below the 1280 byte limit.
	// for _, n := range closest {
	// 	if netutil.CheckRelayIP(from.IP, n.IP()) == nil {
	// 		p.Nodes = append(p.Nodes, nodeToRPC(n))
	// 	}
	// 	if len(p.Nodes) == maxNeighbors {
	// 		t.send(from, neighborsPacket, &p)
	// 		p.Nodes = p.Nodes[:0]
	// 		sent = true
	// 	}
	// }
	// if len(p.Nodes) > 0 || !sent {
	// 	t.send(from, neighborsPacket, &p)
	// }
	return nil
}

func (req *findnode) name() string { return "FINDNODE/v4" }

func (req *neighbors) handle(t *V4Udp, from *net.UDPAddr, fromKey encPubkey, mac []byte) error {
	if expired(req.Expiration) {
		return errExpired
	}
	if !t.handleReply(fromKey.id(), neighborsPacket, incomingPacket{packet: req, recoveredID: fromKey}) {
		return errUnsolicitedReply
	}
	return nil
}

func (req *neighbors) name() string { return "NEIGHBORS/v4" }

func expired(ts uint64) bool {
	return time.Unix(int64(ts), 0).Before(time.Now())
}

package main

import (
	"crypto/ecdsa"
	"flag"
	"net"
	"os"
	"testing"

	"github.com/ShyftNetwork/go-empyrean/cmd/utils"
	"github.com/ShyftNetwork/go-empyrean/crypto"
	"github.com/ShyftNetwork/go-empyrean/p2p/enode"
	"github.com/ShyftNetwork/go-empyrean/p2p/nat"
	"github.com/ShyftNetwork/go-empyrean/p2p/netutil"
	docker "github.com/fsouza/go-dockerclient"
)

var (
	listenPort   *string        // udp listen port
	natdesc      *string        //nat mode
	targetnode   *enode.Node    // parsed Node
	targetIP     net.IP         //targetIP
	dockerHost   *string        //docker host api endpoint
	daemon       *docker.Client //docker daemon proxy
	targetID     *string        //docker client id
	nodeKey      *ecdsa.PrivateKey
	err          error
	restrictList *netutil.Netlist
	v4udp        V4Udp
)

func TestMain(m *testing.M) {

	testTarget := flag.String("enodeTarget", "", "Enode address of target")
	testTargetIP := flag.String("targetIP", "", "IP Address of hive container client")
	listenPort = flag.String("listenPort", ":30303", "")
	natdesc = flag.String("nat", "none", "port mapping mechanism (any|none|upnp|pmp|extip:<IP>)")
	dockerHost = flag.String("dockerHost", "", "docker host api endpoint")
	targetID = flag.String("targetID", "", "the hive client container id")
	flag.Parse()

	//If an enode was supplied, use that
	if *testTarget != "" {
		targetnode, err = enode.ParseV4(*testTarget)
		if err != nil {
			panic(err)
		}
	}

	//If a target ip was supplied, parse it and use it
	if *testTargetIP != "" {
		targetIP = net.ParseIP(*testTargetIP)
		//if the target enode was supplied, override the ip address with the target ip supplied, which
		//seems to be useful when the supplied enode ip address is incorrect in some way when reported
		//from a docker container
		if targetnode != nil {
			targetnode = enode.NewV4(targetnode.Pubkey(), targetIP, targetnode.TCP(), targetnode.UDP())
		}
	}

	//Exit if no args supplied
	if *testTargetIP == "" && targetnode == nil {
		panic("No target enode or ip supplied")
	}

	os.Exit(m.Run())
}

//not currently necessary:
func connectToDockerDaemon(t *testing.T) {
	// this test suite needs to be able to control the client container to:
	// - Reset the container so that nodes are known/unknown
	// - Manipulate faketime for timing related tests
	daemon, err = docker.NewClient(*dockerHost)
	if err != nil {
		t.Error("failed to connect to docker daemon")
		return
	}
	env, err := daemon.Version()
	if err != nil {
		t.Fatalf("failed to retrieve docker version %s", err)
		return
	}
	t.Logf("Daemon with version %s is up", env.Get("Version"))
}

// TestDiscovery tests the set of discovery protocols
func TestDiscovery(t *testing.T) {
	// discovery v4 test suites

	t.Run("discoveryv4", func(t *testing.T) {
		//setup
		v4udp = setupv4UDP()

		//If the client has a known enode, obtained from an admin API, then run a standard ping
		//Otherwise, run a different ping where we override any enode validation checks
		//The recovered id can be used to set the target node id for any further tests that might want to verify that.
		var pingTest func(t *testing.T)

		if targetnode == nil {
			pingTest = SourceUnknownPingUnknownEnode
		} else {
			pingTest = SourceUnknownPingKnownEnode
		}

		t.Run("pingTest(v4001)", pingTest)
		t.Run("SourceUnknownPingWrongTo(v4002)", SourceUnknownPingWrongTo)
		t.Run("SourceUnknownPingWrongFrom(v4003)", SourceUnknownPingWrongFrom)
		t.Run("SourceUnknownPingExtraData(v4004)", SourceUnknownPingExtraData)
		t.Run("SourceUnknownPingExtraDataWrongFrom(v4005)", SourceUnknownPingExtraDataWrongFrom)
		t.Run("SourceUnknownWrongPacketType(v4006)", SourceUnknownWrongPacketType)
		t.Run("SourceUnknownFindNeighbours(v4007)", SourceUnknownFindNeighbours)

		t.Run("SourceKnownPingFromSignatureMismatch(v4009)", SourceKnownPingFromSignatureMismatch)
		t.Run("FindNeighboursOnRecentlyBondedTarget(v4010)", FindNeighboursOnRecentlyBondedTarget)
		t.Run("PingPastExpiration(v4011)", PingPastExpiration)
		t.Run("FindNeighboursPastExpiration(v4012)", FindNeighboursPastExpiration)

	})

	t.Run("discoveryv5", func(t *testing.T) {

		t.Run("ping", func(t *testing.T) {
			//TODO
		})
	})

}

//v4001a
func SourceUnknownPingUnknownEnode(t *testing.T) {
	t.Log("Pinging unknown node id.")
	if err := v4udp.ping(enode.ID{}, &net.UDPAddr{IP: targetIP, Port: 30303}, false, func(e *ecdsa.PublicKey) {

		targetnode = enode.NewV4(e, targetIP, 30303, 30303)
		t.Log("Discovered node id " + targetnode.String())
	}); err != nil {
		t.Fatalf("Unable to v4 ping: %v", err)
	}
}

//v4001b
func SourceUnknownPingKnownEnode(t *testing.T) {
	t.Log("Test v4001")
	if err := v4udp.ping(targetnode.ID(), &net.UDPAddr{IP: targetnode.IP(), Port: targetnode.UDP()}, true, nil); err != nil {
		t.Fatalf("Ping test failed: %v", err)
	}
}

//v4002
func SourceUnknownPingWrongTo(t *testing.T) {
	t.Log("Test v4002")
	if err := v4udp.pingWrongTo(targetnode.ID(), &net.UDPAddr{IP: targetnode.IP(), Port: targetnode.UDP()}, true, nil); err != nil {
		t.Fatalf("Test failed: %v", err)
	}

}

//v4003
func SourceUnknownPingWrongFrom(t *testing.T) {
	t.Log("Test v4003")
	if err := v4udp.pingWrongFrom(targetnode.ID(), &net.UDPAddr{IP: targetnode.IP(), Port: targetnode.UDP()}, true, nil); err != nil {
		t.Fatalf("Test failed: %v", err)
	}
}

//v4004
func SourceUnknownPingExtraData(t *testing.T) {
	t.Log("Test v4004")
	if err := v4udp.pingExtraData(targetnode.ID(), &net.UDPAddr{IP: targetnode.IP(), Port: targetnode.UDP()}, true, nil); err != nil {
		t.Fatalf("Test failed: %v", err)
	}
}

//v4005
func SourceUnknownPingExtraDataWrongFrom(t *testing.T) {
	t.Log("Test v4005")
	if err := v4udp.pingExtraDataWrongFrom(targetnode.ID(), &net.UDPAddr{IP: targetnode.IP(), Port: targetnode.UDP()}, true, nil); err != nil {
		t.Fatalf("Test failed: %v", err)
	}
}

//v4006
func SourceUnknownWrongPacketType(t *testing.T) {
	t.Log("Test v4006")
	if err := v4udp.pingTargetWrongPacketType(targetnode.ID(), &net.UDPAddr{IP: targetnode.IP(), Port: targetnode.UDP()}, true, nil); err != errTimeout {
		t.Fatalf("Test failed: %v", err)
	}
}

//v4007
func SourceUnknownFindNeighbours(t *testing.T) {
	t.Log("Test v4007")
	targetEncKey := encodePubkey(targetnode.Pubkey())
	if err := v4udp.findnodeWithoutBond(targetnode.ID(), &net.UDPAddr{IP: targetnode.IP(), Port: targetnode.UDP()}, targetEncKey); err != errTimeout {
		t.Fatalf("Test failed: %v", err)
	}
}

//v4009
func SourceKnownPingFromSignatureMismatch(t *testing.T) {

	t.Log("Test v4009")
	if err := v4udp.pingBondedWithMangledFromField(targetnode.ID(), &net.UDPAddr{IP: targetnode.IP(), Port: targetnode.UDP()}, true, nil); err != nil {
		t.Fatalf("Test failed: %v", err)
	}

}

//v4010
func FindNeighboursOnRecentlyBondedTarget(t *testing.T) {
	t.Log("Test v4010")
	targetEncKey := encodePubkey(targetnode.Pubkey())
	if err := v4udp.bondedSourceFindNeighbours(targetnode.ID(), &net.UDPAddr{IP: targetnode.IP(), Port: targetnode.UDP()}, targetEncKey); err != nil {
		t.Fatalf("Test failed: %v", err)
	}
}

//v4011
func PingPastExpiration(t *testing.T) {
	t.Log("Test v4011")
	if err := v4udp.pingPastExpiration(targetnode.ID(), &net.UDPAddr{IP: targetnode.IP(), Port: targetnode.UDP()}, true, nil); err != errTimeout {
		t.Fatalf("Test failed: %v", err)
	}
}

//v4012
func FindNeighboursPastExpiration(t *testing.T) {
	t.Log("Test v4012")
	targetEncKey := encodePubkey(targetnode.Pubkey())
	if err := v4udp.bondedSourceFindNeighboursPastExpiration(targetnode.ID(), &net.UDPAddr{IP: targetnode.IP(), Port: targetnode.UDP()}, targetEncKey); err != errTimeout {
		t.Fatalf("Test failed: %v", err)
	}
}

// TestRLPx checks the RLPx handshaking
func TestRLPx(t *testing.T) {
	// discovery v4 test suites
	t.Run("connect", func(t *testing.T) {
		//
		t.Run("basic", func(t *testing.T) {

		})
	})

}

func setupv4UDP() V4Udp {
	//Resolve an address (eg: ":port") to a UDP endpoint.
	addr, err := net.ResolveUDPAddr("udp", *listenPort)
	if err != nil {
		panic(err)
	}

	//Create a UDP connection
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		utils.Fatalf("-ListenUDP: %v", err)
	}

	//FS: The following just gets the local address, does something with NAT and converts into a
	//general address type.
	natm, err := nat.Parse(*natdesc)
	if err != nil {
		utils.Fatalf("-nat: %v", err)
	}
	realaddr := conn.LocalAddr().(*net.UDPAddr)
	if natm != nil {
		if !realaddr.IP.IsLoopback() {
			go nat.Map(natm, nil, "udp", realaddr.Port, realaddr.Port, "ethereum discovery")
		}
		// TODO: react to external IP changes over time.
		if ext, err := natm.ExternalIP(); err == nil {
			realaddr = &net.UDPAddr{IP: ext, Port: realaddr.Port}
		}
	}

	nodeKey, err = crypto.GenerateKey()

	if err != nil {
		utils.Fatalf("could not generate key: %v", err)
	}

	cfg := Config{
		PrivateKey:   nodeKey,
		AnnounceAddr: realaddr,
		NetRestrict:  restrictList,
	}

	var v4UDP *V4Udp

	if v4UDP, err = ListenUDP(conn, cfg); err != nil {
		panic(err)
	}

	return *v4UDP
}

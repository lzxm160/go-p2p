package p2p

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"strconv"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-crypto"
	"github.com/libp2p/go-libp2p-host"
	"github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p-net"
	"github.com/libp2p/go-libp2p-peer"
	"github.com/libp2p/go-libp2p-peerstore"
	"github.com/libp2p/go-libp2p-protocol"
	"github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p-transport-upgrader"
	"github.com/libp2p/go-tcp-transport"
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multihash"
	"github.com/whyrusleeping/go-smux-yamux"
	"go.uber.org/zap"
)

// HandleBroadcast defines the callback function triggered when a broadcast message reaches a host
type HandleBroadcast func(data []byte) error

// HandleUnicast defines the callback function triggered when a unicast message reaches a host
type HandleUnicast func(data []byte) error

// Config enumerates the configs required by a host
type Config struct {
	HostName         string
	Port             int
	ExternalHostName string
	ExternalPort     int
	SecureIO         bool
	Gossip           bool
	ConnectTimeout   time.Duration
}

// DefaultConfig is a set of default configs
var DefaultConfig = Config{
	HostName:         "127.0.0.1",
	Port:             30001,
	ExternalHostName: "",
	ExternalPort:     30001,
	SecureIO:         false,
	Gossip:           false,
	ConnectTimeout:   time.Minute,
}

// Option defines the option function to modify the config for a host
type Option func(cfg *Config) error

// HostName is the option to override the host name or IP address
func HostName(hostName string) Option {
	return func(cfg *Config) error {
		cfg.HostName = hostName
		return nil
	}
}

// Port is the option to override the port number
func Port(port int) Option {
	return func(cfg *Config) error {
		cfg.Port = port
		return nil
	}
}

// ExternalHostName is the option to set the host name or IP address seen from external
func ExternalHostName(externalHostName string) Option {
	return func(cfg *Config) error {
		cfg.ExternalHostName = externalHostName
		return nil
	}
}

// ExternalPort is the option to set the port number seen from external
func ExternalPort(externalPort int) Option {
	return func(cfg *Config) error {
		cfg.ExternalPort = externalPort
		return nil
	}
}

// SecureIO is to indicate using secured I/O
func SecureIO() Option {
	return func(cfg *Config) error {
		cfg.SecureIO = true
		return nil
	}
}

// Gossip is to indicate using gossip protocol
func Gossip() Option {
	return func(cfg *Config) error {
		cfg.Gossip = true
		return nil
	}
}

// ConnectTimeout is the option to override the connect timeout
func ConnectTimeout(timout time.Duration) Option {
	return func(cfg *Config) error {
		cfg.Gossip = true
		return nil
	}
}

// Host is the main struct that represents a host that communicating with the rest of the P2P networks
type Host struct {
	host      host.Host
	cfg       Config
	ctx       context.Context
	topics    map[string]interface{}
	kad       *dht.IpfsDHT
	kadKey    cid.Cid
	newPubSub func(ctx context.Context, h host.Host, opts ...pubsub.Option) (*pubsub.PubSub, error)
	pubs      map[string]*pubsub.PubSub
	subs      map[string]*pubsub.Subscription
	close     chan interface{}
}

// NewHost constructs a host struct
func NewHost(ctx context.Context, options ...Option) (*Host, error) {
	cfg := DefaultConfig
	for _, option := range options {
		if err := option(&cfg); err != nil {
			return nil, err
		}
	}
	ip, err := EnsureIPv4(cfg.HostName)
	if err != nil {
		return nil, err
	}
	sk, _, err := generateKeyPair(fmt.Sprintf("%s:%d", ip, cfg.Port))
	if err != nil {
		return nil, err
	}
	var extMultiAddr multiaddr.Multiaddr
	// Set external address and replace private key it external host name is given
	if cfg.ExternalHostName != "" {
		extIP, err := EnsureIPv4(cfg.ExternalHostName)
		if err != nil {
			return nil, err
		}
		sk, _, err = generateKeyPair(fmt.Sprintf("%s:%d", extIP, cfg.ExternalPort))
		if err != nil {
			return nil, err
		}
		extMultiAddr, err = multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%d", extIP, cfg.ExternalPort))
		if err != nil {
			return nil, err
		}
	}
	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(fmt.Sprintf("/ip4/%s/tcp/%d", ip, cfg.Port)),
		libp2p.AddrsFactory(func(addrs []multiaddr.Multiaddr) []multiaddr.Multiaddr {
			if extMultiAddr != nil {
				return append(addrs, extMultiAddr)
			}
			return addrs
		}),
		libp2p.Identity(sk),
		libp2p.Transport(func(upgrader *stream.Upgrader) *tcp.TcpTransport {
			return &tcp.TcpTransport{Upgrader: upgrader, ConnectTimeout: cfg.ConnectTimeout}
		}),
		libp2p.Muxer("/yamux/2.0.0", sm_yamux.DefaultTransport),
		libp2p.EnableRelay(),
		libp2p.NATPortMap(),
	}
	if !cfg.SecureIO {
		opts = append(opts, libp2p.NoSecurity)
	}
	host, err := libp2p.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	kad, err := dht.New(ctx, host)
	if err != nil {
		return nil, err
	}
	newPubSub := pubsub.NewFloodSub
	if cfg.Gossip {
		newPubSub = pubsub.NewGossipSub
	}
	v1b := cid.V1Builder{Codec: cid.Raw, MhType: multihash.SHA2_256}
	cid, err := v1b.Sum([]byte(host.ID().Pretty()))
	if err != nil {
		return nil, err
	}
	// Update actual port if it's set to 0
	if cfg.Port == 0 {
		addr := host.Addrs()[0]
		portStr, err := addr.ValueForProtocol(multiaddr.P_TCP)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(portStr)
		cfg.Port = port
	}
	log.Println("address: ", host.Addrs())
	myHost := Host{
		host:      host,
		cfg:       cfg,
		ctx:       ctx,
		topics:    make(map[string]interface{}),
		kad:       kad,
		kadKey:    cid,
		newPubSub: newPubSub,
		pubs:      make(map[string]*pubsub.PubSub),
		subs:      make(map[string]*pubsub.Subscription),
		close:     make(chan interface{}),
	}
	Logger().Info("P2p host started.",
		zap.String("address", myHost.Address()),
		zap.String("multiAddress", myHost.MultiAddress()),
		zap.Bool("secureIO", myHost.cfg.SecureIO),
		zap.Bool("gossip", myHost.cfg.Gossip))
	return &myHost, nil
}

// Connect connects a peer given the address string
func (h *Host) Connect(addr string) error {
	ma, err := addrToMultiAddr(addr)
	if err != nil {
		return err
	}
	target, err := peerstore.InfoFromP2pAddr(ma)
	if err != nil {
		return err
	}
	if err := h.host.Connect(h.ctx, *target); err != nil {
		return err
	}
	Logger().Debug("P2P peer connected.",
		zap.String("address", addr),
		zap.String("multiAddress", ma.String()))
	return nil
}

// JoinOverlay triggers the host to join the DHT overlay
func (h *Host) JoinOverlay() error {
	if err := h.kad.Provide(h.ctx, h.kadKey, true); err == nil {
		return nil
	}
	return h.kad.Bootstrap(h.ctx)
}

// AddUnicastPubSub adds a unicast topic that the host will pay attention to
func (h *Host) AddUnicastPubSub(topic string, callback HandleUnicast) error {
	if _, ok := h.topics[topic]; ok {
		return nil
	}
	h.host.SetStreamHandler(protocol.ID(topic), func(stream net.Stream) {
		defer func() {
			if err := stream.Close(); err != nil {
				Logger().Error("Error when closing a unicast stream.", zap.Error(err))
			}
		}()
		data, err := ioutil.ReadAll(stream)
		if err != nil {
			Logger().Error("Error when subscribing a unicast message.", zap.Error(err))
			return
		}
		if err := callback(data); err != nil {
			Logger().Error("Error when processing a unicast message.", zap.Error(err))
		}
	})
	h.topics[topic] = nil
	return nil
}

// AddBroadcastPubSub adds a broadcast topic that the host will pay attention to. This need to be called before using
// Connect/JoinOverlay. Otherwise, pubsub may not be aware of the existing overlay topology
func (h *Host) AddBroadcastPubSub(topic string, callback HandleBroadcast) error {
	if _, ok := h.pubs[topic]; ok {
		return nil
	}
	pub, err := h.newPubSub(h.ctx, h.host)
	if err != nil {
		return err
	}
	sub, err := pub.Subscribe(topic)
	if err != nil {
		return err
	}
	h.pubs[topic] = pub
	h.subs[topic] = sub
	go func() {
		for {
			select {
			case <-h.close:
				return
			default:
				msg, err := sub.Next(h.ctx)
				if err != nil {
					Logger().Error("Error when subscribing a broadcast message.", zap.Error(err))
					continue
				}
				if err := callback(msg.Data); err != nil {
					Logger().Error("Error when processing a broadcast message.", zap.Error(err))
				}
			}
		}
	}()
	return nil
}

// Broadcast sends a message to the hosts who subscribe the topic
func (h *Host) Broadcast(topic string, data []byte) error {
	pub, ok := h.pubs[topic]
	if !ok {
		return nil
	}
	return pub.Publish(topic, data)
}

// Unicast sends a message to a peer on the given address
func (h *Host) Unicast(addr string, topic string, data []byte) (err error) {
	ma, err := addrToMultiAddr(addr)
	if err != nil {
		return err
	}
	peerIDStr, err := ma.ValueForProtocol(multiaddr.P_IPFS)
	if err != nil {
		return err
	}
	peerID, err := peer.IDB58Decode(peerIDStr)
	if err != nil {
		return err
	}
	if len(h.host.Peerstore().Addrs(peerID)) == 0 {
		h.host.Peerstore().AddAddr(peerID, ma, peerstore.TempAddrTTL)
	}
	stream, err := h.host.NewStream(h.ctx, peerID, protocol.ID(topic))
	if err != nil {
		return err
	}
	defer func() { err = stream.Close() }()
	if _, err = stream.Write(data); err != nil {
		return err
	}
	return nil
}

// Identity returns the identity string
func (h *Host) Identity() string {
	return h.host.ID().Pretty()
}

// IdentityHash returns the identity hash string
func (h *Host) IdentityHash() string { return h.kadKey.String() }

// Address return the canonical network address
func (h *Host) Address() string { return fmt.Sprintf("%s:%d", h.cfg.HostName, h.cfg.Port) }

// MultiAddress returns the multi address
func (h *Host) MultiAddress() string {
	addr := h.host.Addrs()[0]
	hostAddr, _ := multiaddr.NewMultiaddr(fmt.Sprintf("/ipfs/%s", h.Identity()))
	return addr.Encapsulate(hostAddr).String()
}

// Neighbors returns the closest peer addresses
func (h *Host) Neighbors() ([]string, error) {
	peers, err := h.kad.GetClosestPeers(h.ctx, h.kadKey.String())
	if err != nil {
		return nil, err
	}
	neighbors := make([]string, 0)
	for peer := range peers {
		for _, ma := range h.kad.FindLocal(peer).Addrs {
			ip, err := ma.ValueForProtocol(multiaddr.P_IP4)
			if ip == "" || err != nil {
				continue
			}
			portStr, err := ma.ValueForProtocol(multiaddr.P_TCP)
			if err != nil {
				continue
			}
			port, err := strconv.Atoi(portStr)
			if err != nil {
				continue
			}
			neighbors = append(neighbors, fmt.Sprintf("%s:%d", ip, port))
			break
		}
	}
	return neighbors, nil
}

// Close closes the host
func (h *Host) Close() error {
	close(h.close)
	for _, sub := range h.subs {
		sub.Cancel()
	}
	if err := h.kad.Close(); err != nil {
		return err
	}
	if err := h.host.Close(); err != nil {
		return err
	}
	return nil
}

// generateKeyPair generates the public key and private key by network address
func generateKeyPair(addr string) (crypto.PrivKey, crypto.PubKey, error) {
	hash := sha1.Sum([]byte(addr))
	seedBytes := hash[12:]
	seedBytes[0] = 0
	seed := int64(binary.BigEndian.Uint64(seedBytes))
	r := rand.New(rand.NewSource(seed))
	return crypto.GenerateKeyPairWithReader(crypto.Ed25519, 2048, r)
}

func addrToMultiAddr(addr string) (multiaddr.Multiaddr, error) {
	return multiaddr.NewMultiaddr(addr)
	//parts := strings.Split(addr, ":")
	//if len(parts) != 2 {
	//return nil, fmt.Errorf("%s is not a valid address", addr)
	//}
	//// TODO: doesn't support the case of multi IPs binding to the same host name
	//ip, err := EnsureIPv4(parts[0])
	//if err != nil {
	//return nil, err
	//}
	//port, err := strconv.Atoi(parts[1])
	//if err != nil {
	//return nil, err
	//}
	//_, pk, err := generateKeyPair(fmt.Sprintf("%s:%d", ip, port))
	//if err != nil {
	//return nil, err
	//}
	//id, err := peer.IDFromPublicKey(pk)
	//if err != nil {
	//return nil, err
	//}
	//maStr := fmt.Sprintf("/ip4/%s/tcp/%d/ipfs/%s", ip, port, id.Pretty())
	//return multiaddr.NewMultiaddr(maStr)
}

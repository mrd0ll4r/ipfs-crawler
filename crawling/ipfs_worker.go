package crawling

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/host"
	pb "github.com/libp2p/go-libp2p-kad-dht/pb"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-msgio"

	"github.com/libp2p/go-libp2p-core/protocol"
	"github.com/libp2p/go-msgio/protoio"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

type CrawlerConfig struct {
	MaxBackOffTime  int
	ConnectTimeout  time.Duration
	QueueSize       int
	ProtocolStrings []protocol.ID `mapstructure:"protocolStrings"`
	UserAgent       string
}

// TODO: number of buckets = connectTimeout
var connectDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "ipfs_crawler_worker_connect_duration_seconds",
	Help:    "Histogram for the duration of Connect().",
	Buckets: prometheus.LinearBuckets(0, 1, 45),
})

var rawNewIDs = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "ipfs_crawler_worker_raw_obtained_IDs_per_peer",
	Help:    "Raw number of obtained IDs from the ipfs_worker per crawled peer. Does not exclude previously seen IDs.",
	Buckets: prometheus.LinearBuckets(0, 300, 10),
})

// TODO: Refer ConnectionTimeout

func init() {
	// Register metrics with prometheus
	prometheus.MustRegister(connectDuration)
	prometheus.MustRegister(rawNewIDs)

	// Set defaults
	viper.SetDefault("maxBackOffTime", 500)
	viper.SetDefault("connectTimeout", 45*time.Second)
	viper.SetDefault("protocolStrings", []protocol.ID{
		"/ipfs/kad/1.0.0",
		"/ipfs/kad/2.0.0",
	})
}

func configure() CrawlerConfig {
	var config CrawlerConfig
	err := viper.UnmarshalKey("worker", &config)
	if err != nil {
		panic(err)
	}
	return config
}

// PrefixLimitError signals that we have exhausted the bucket space.
type PrefixLimitError struct {
	msg  string
	peer peer.AddrInfo
}

func (e *PrefixLimitError) Error() string {
	return e.msg
}

// IPFSWorker performs the connection and extracting the dht buckets from remote nodes.
type IPFSWorker struct {
	id      int
	ph      *PreImageHandler
	quitMsg chan bool
	h       host.Host
	ctx     context.Context // ToDo: Find a way around storing this context explicitly, handle it in the loop maybe?
	// https://www.reddit.com/r/golang/comments/75vowy/question_is_it_ok_to_store_a_contextcancelfunc/do9kjqz/
	cancelFunc    context.CancelFunc
	crawlErrors   int
	crawlAttempts int
	resultChannel chan peer.AddrInfo
	config        CrawlerConfig
	capacity      int
	Events        *EventManager
}

// NodeKnows stores the collected addresses for a given ID
type NodeKnows struct {
	id    peer.ID
	knows []*peer.AddrInfo
	info  map[string]interface{}
}

// NewWorker initiates a new instance of a crawl worker.
// Initalizes the token bucket used for rate limiting and the necessary RSA keys for IPFS
//
// :param cm: Instance of the crawlmanager the new worker will be attached to
// :param id: ID of the new worker
// :param ctx: context that the new worker will be attached to
// :return: fully initialized worker
func NewIPFSWorker(id int, ctx context.Context) *IPFSWorker {
	// ToDo: Not sure if we should 1) derive a new context 2) store the context
	config := configure()
	ctx, cancel := context.WithCancel(ctx)
	w := &IPFSWorker{
		id:            id,
		ph:            nil,
		quitMsg:       make(chan bool),
		ctx:           ctx,
		cancelFunc:    cancel,
		resultChannel: make(chan peer.AddrInfo, 1000),
		config:        config,
		capacity:      config.QueueSize,
		Events:        NewEventManager(),
	}

	// Init the host, i.e., generate priv key and all that stuff
	priv, _, _ := crypto.GenerateKeyPair(crypto.RSA, 2048)
	opts := []libp2p.Option{libp2p.Identity(priv), libp2p.UserAgent(config.UserAgent)}
	h, err := libp2p.New(ctx, opts...)
	if err != nil {
		panic(err)
	}
	w.h = h

	return w
}

func (w *IPFSWorker) GetHost() host.Host {
	return w.h
}

func (w *IPFSWorker) SetHost(h host.Host) {
	w.h = h
}

func (w *IPFSWorker) CrawlPeer(askPeer *peer.AddrInfo) (*NodeKnows, error) {
	// Strip addresses we cannot connect to anyways
	recvPeer := stripLocalAddrs(*askPeer)
	log.WithFields(log.Fields{
		"IPFSWorkerID": w.id,
		"destAddr":     recvPeer,
	}).Debug("IPFSWorker connecting to")
	// Check if there are an addresses left
	if len(recvPeer.Addrs) == 0 {
		// Nope
		return nil, fmt.Errorf("ID: %s has only local adresses", askPeer.ID)
	}
	time.Sleep(time.Duration(rand.Intn(w.config.MaxBackOffTime)) * time.Millisecond)

	// Roadmap:
	// 1) Connect to peer
	// 2) Start a new stream = subprotocol exchange
	// 3) Send FindNode(s)
	// 4) Parse response, add to Queue
	ctx, cancel := context.WithTimeout(w.ctx, w.config.ConnectTimeout)
	defer cancel()
	// Connect() adheres to the context deadline and gives and error when the context deadline expired
	// ToDo: It seems that this is ignored when the context previously expired

	connTimer := prometheus.NewTimer(connectDuration)
	err := w.h.Connect(ctx, recvPeer)
	connTimer.ObserveDuration()
	if err != nil {
		// We couldn't connect to the target peer. This is either because it's unreachable or the context timed out.
		// In that case, we give up and consider the peer as unreachable.
		log.WithFields(log.Fields{
			"IPFSWorkerID": w.id,
			"err":          err,
			"destAddr":     recvPeer,
		}).Debug("Could not connect.")
		return nil, err
	}

	// Create a new stream
	// Whereas NewStream() does not care if the context timed out.
	dhtStream, err := w.h.NewStream(ctx, recvPeer.ID, w.config.ProtocolStrings...)
	if err != nil {
		// ToDo: Better error handling
		log.WithFields(log.Fields{
			"IPFSWorkerID": w.id,
			"err":          err,
			"destAddr":     recvPeer,
		}).Debug("Could not open stream.")
		return nil, err
	}
	defer dhtStream.Close()

	returnedPeers, err := w.FullNeighborCrawl(ctx, dhtStream, recvPeer, w.ph)
	if err != nil {
		log.WithFields(log.Fields{
			"IPFSWorkerID": w.id,
			"err":          err,
			"destAddr":     recvPeer,
		}).Debug("Error sending crawl msg.")
		// If there are still some peers that we learned of then we deal with them in the normal way, despite the error.
		// If there are no peers, there's no hope.
		if len(returnedPeers) == 0 {
			return nil, err
		}
	}

	// Get agent version from Peerstore
	// Returns the value (more exactly and Interface) and potentially an error
	var av string
	agentVersion, err := w.h.Peerstore().Get(recvPeer.ID, "AgentVersion")
	if err == nil {
		av = agentVersion.(string)
	}
	log.WithFields(log.Fields{
		"IPFSWorkerID": w.id,
	}).Debug("Fire connected callbacks")
	w.Events.Emit("connected", recvPeer)
	infos := make(map[string]interface{})
	infos["version"] = av

	// Get stream protocol. Return type is protocol.ID which is an alias for string
	streamProtocol := dhtStream.Protocol()
	infos["protocol"] = streamProtocol
	infos["knows_timestamp"] = time.Now().Format("2006-01-02T15:04:05-0700")
	rawNewIDs.Observe(float64(len(returnedPeers)))
	return &NodeKnows{id: recvPeer.ID, knows: returnedPeers, info: infos}, nil
}

func (w *IPFSWorker) AddPreimages(handler *PreImageHandler) {
	w.ph = handler
}

func (w *IPFSWorker) Capacity() int {
	return w.capacity
}

// FullNeighborCrawl systematically reads the dht buckets from remote node.
//
// Asks the remote node for the closest peers to a given prefix the remote knows.
// Iterates through the prefixes until no new peers are learned.
// raises an exception if prefix space is exhausted.
//
// :param ctx: controlling context
// :param remotePeerStream: open connection to remote node
// :param remotePeerInfo: Address of the remote node
// :param ph: Lookup table for prefixes
// :return: slice of learned adresses
func (w *IPFSWorker) FullNeighborCrawl(ctx context.Context, remotePeerStream network.Stream,
	remotePeerInfo peer.AddrInfo, ph *PreImageHandler,
) ([]*peer.AddrInfo, error) {
	// Send the FindNode packet. Here it goes.
	// Start with a common prefix length of 0 and successively move to closer IDs until we either
	// learn no new peers or our hard cap for the CPL pre-computation is reached.
	var returnedPeers []*peer.AddrInfo
	seenIDs := make(map[peer.ID]bool)
	var newlyLearnedPeers int

	recvReader := msgio.NewVarintReaderSize(remotePeerStream, network.MessageSizeMax)
	// This closes the whole stream (!)
	defer recvReader.Close()

	var i int
	// Ask at least 4 times
	for i = 0; (i < 4 || newlyLearnedPeers != 0) && (i < 24); i++ {
		newlyLearnedPeers = 0
		target := ph.FindPreImageForCPL(remotePeerInfo, uint8(i))
		log.WithFields(log.Fields{
			"IPFSWorkerID": w.id,
			"cpl":          i,
			"destAddr":     remotePeerInfo,
		}).Trace("Sending FindNode.")

		peerResponse, err := SendFindNode(ctx, recvReader, target, remotePeerStream)
		if err != nil {
			// ToDo: Better error handling. E.g. try the loop again, create a new context for that
			switch err {
			case context.DeadlineExceeded:
				return returnedPeers, err
			default:
				return returnedPeers, err
			}
		}
		for _, p := range peerResponse {
			if _, ok := seenIDs[p.ID]; ok {
				continue
			}
			returnedPeers = append(returnedPeers, p)
			seenIDs[p.ID] = true
			newlyLearnedPeers++
		}
		log.WithFields(log.Fields{
			"IPFSWorkerID":    w.id,
			"numLearnedPeers": newlyLearnedPeers,
		}).Trace("IPFSWorker learned peers.")
	}
	if i == 23 {
		// Return that we reached the prefix limit, so this can be tracked.
		return returnedPeers, &PrefixLimitError{
			msg:  "Prefix limit reached.",
			peer: remotePeerInfo,
		}
	} else {
		// Everything went well
		return returnedPeers, nil
	}
}

// Stop stops the IPFSWorker.
func (w *IPFSWorker) Stop() {
	// w.dht.Close()
	var errRatio int // Don't care about precision, #yolo
	if w.crawlAttempts != 0 {
		errRatio = w.crawlErrors * 100 / w.crawlAttempts
	} else {
		errRatio = 0
	}

	log.WithFields(log.Fields{
		"IPFSWorkerID":     w.id,
		"crawlErrors":      w.crawlErrors,
		"crawlAttempts":    w.crawlAttempts,
		"failedPercentage": errRatio,
	}).Info("IPFSWorker finished with stats.")
	w.cancelFunc()
	w.quitMsg <- true
}

// SendFindNode probes the remote node for neighborhood nodes.
// :param ctx: controlling context
// :param recvReader: Reader/parser for the responses
// :param target: the prefix we are interested in
// :param remotePeerStream: Connection to remote node
// :return: list of received peer adresses
func SendFindNode(ctx context.Context, recvReader msgio.Reader, target []byte, remotePeerStream network.Stream) ([]*peer.AddrInfo, error) {
	// Send the packet to the target host and wait for the response or context timeout
	err := protoio.NewDelimitedWriter(remotePeerStream).WriteMsg(pb.NewMessage(pb.Message_FIND_NODE, target, 0))
	if err != nil {
		// This can fail, since we're sending multiple packets on the same stream.
		// If it does, for now we just ignore the problem and return the error.
		// The higher levels should deal with this
		log.WithField("err", err).Warn("Sending findnode failed.")
		return nil, err
	}

	// Receive the response and handle it accordingly
	var response pb.Message

	// The ReadMsg() function is synchronous, so we use this little async wrapper, s.t. we can adhere to the context timeout
	errChan := make(chan error, 1)
	responseChan := make(chan []byte, 1)

	go func() {
		msgbytes, err := recvReader.ReadMsg()
		if err != nil {
			errChan <- err
		} else {
			responseChan <- msgbytes
		}
	}()

	select {
	case <-ctx.Done():
		// The context timed out, abort sendin/receiving and return.
		return nil, ctx.Err()

	case msg := <-responseChan:
		// Parse the request and then signal that the msgbytes-buffer can be used again
		response.Unmarshal(msg)
		// ToDo: Is this copied or just by reference? In a good language that would be more clear...
		recvReader.ReleaseMsg(msg)
		return pb.PBPeersToPeerInfos(response.GetCloserPeers()), nil

	case err := <-errChan:
		return nil, err

	}
}

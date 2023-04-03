package crawling

import (
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var promMetricWaitingForRequests = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "ipfs_crawler_cmanager_waiting_for_request_queue_length",
	Help: "Current number of requests that are awaiting responses.",
})

var promMetricNumberOfNewIDs = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "ipfs_crawler_cmanager_number_new_IDs",
	Help: "Current number of newly learned node IDs.",
},
	[]string{
		"reachable",
	})

var promMetricTokenBucketLength = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "ipfs_crawler_cmanager_token_bucket_free_capacity",
	Help: "Free capacity of the token bucket used to rate limit the crawl.",
})

// Set defaults for CrawlManager
func init() {
	prometheus.MustRegister(promMetricWaitingForRequests)
	prometheus.MustRegister(promMetricNumberOfNewIDs)
	prometheus.MustRegister(promMetricTokenBucketLength)

	// TODO: sort out necessary defaults
	viper.SetDefault("FilenameTimeFormat", "02-01-06--15:04:05")
	viper.SetDefault("OutPath", "output_data_crawls/")
	viper.SetDefault("WriteToFileFlag", true)
	viper.SetDefault("CanaryFile", "configs/canary.txt")
	viper.SetDefault("Sanity", false)
}

type CMOutputConfig struct {
	WriteToFileFlag    bool   `mapstructure:"dataOutputEnabled""`
	OutPath            string `mapstructure:"outpath""`
	FilenameTimeFormat string `mapstructure:"filenameTimeFormat""`
}

// Config Object for CrawlManager
type CrawlManagerConfig struct {
	Output     CMOutputConfig `mapstructure:"dataOutput"`
	CanaryFile string         `mapstructure:"canaryfile"`
	Sanity     bool           `mapstructure:"sanityEnabled"`
}

func configureCrawlerManager() CrawlManagerConfig {
	var config CrawlManagerConfig

	err := viper.UnmarshalKey("crawloptions", &config)
	if err != nil {
		panic(err)
	}
	return config
}

// Interface for a crawlWorker
type CrawlerWorker interface {
	Capacity() int
	CrawlPeer(*peer.AddrInfo) (*NodeKnows, error)
}

type CrawlOutput struct {
	StartDate string
	EndDate   string
	Nodes     map[peer.ID]*CrawledNode
}

type CrawledNode struct {
	NID          peer.ID
	MultiAddrs   []ma.Multiaddr
	Reachable    bool
	AgentVersion string
	Neighbours   []peer.ID
	Timestamp    string
}

// Container struct for crawl results... because of go...
type CrawlResult struct {
	Node *NodeKnows
	Err  error
}

type CrawlManagerV2 struct {
	queueSize          int
	ReportQueue        chan CrawlResult
	toCrawl            []*peer.AddrInfo
	tokenBucket        chan int
	concurrentRequests int
	// We use this map not only to store whether we crawled a node but also to store a nodes multiaddress
	crawled   map[peer.ID][]ma.Multiaddr
	knows     map[peer.ID][]peer.ID
	online    map[peer.ID]bool
	info      map[peer.ID]map[string]interface{}
	quitMsg   chan bool
	Done      chan bool
	workers   []*CrawlerWorker
	startTime time.Time
	config    CrawlManagerConfig
}

func NewCrawlManagerV2(queueSize int) *CrawlManagerV2 {
	cm := &CrawlManagerV2{
		ReportQueue: make(chan CrawlResult, queueSize),
		tokenBucket: make(chan int, queueSize),
		crawled:     make(map[peer.ID][]ma.Multiaddr),
		online:      make(map[peer.ID]bool),
		knows:       make(map[peer.ID][]peer.ID),
		info:        make(map[peer.ID]map[string]interface{}),
		quitMsg:     make(chan bool),
		Done:        make(chan bool),
		startTime:   time.Now(),
	}
	config := configureCrawlerManager()
	cm.config = config
	return cm
}

func (cm *CrawlManagerV2) AddWorker(w CrawlerWorker) {
	cm.workers = append(cm.workers, &w)
	// get sum and maximum capacity of workers
	// recreate the tokenBucket
	// add tokens round-robin style
	sumCap := 0
	maxCap := 0
	for _, worker := range cm.workers {
		if (*worker).Capacity() > maxCap {
			maxCap = (*worker).Capacity()
		}
		sumCap += (*worker).Capacity()
	}
	log.WithFields(log.Fields{
		"sumCap":   sumCap,
		"maxCap":   maxCap,
		"capacity": w.Capacity(),
	}).Debug("Size of Queue")
	cm.tokenBucket = make(chan int, sumCap)
	cm.ReportQueue = make(chan CrawlResult, sumCap)
	cm.queueSize = sumCap
	for iter := 0; iter < maxCap; iter++ {
		for id, worker := range cm.workers {
			if (*worker).Capacity() >= iter {
				cm.tokenBucket <- id
			}
		}
	}
	log.WithFields(log.Fields{
		"QueueSize": len(cm.tokenBucket),
	}).Debug("Size of Queue")
}

func (cm *CrawlManagerV2) CrawlNetwork(bootstraps []*peer.AddrInfo) *CrawlOutput {
	// Plan of action
	// 1. Add bootstraps to overflow
	// 2. Start dispatch loop
	//  2.1 get new nodes from ReportQueue and check if we need to crawl them, if yes: add to toCrawl
	//  2.2 if we can dispatch a crawl: dispatch from toCrawl
	//  2.3 break loop: idleTimer fired | (toCrawl empty && no request are out && knowQueue empty)
	//  return data TODO: what kind of format
	log.Info("Starting crawl...")
	if len(cm.workers) < 1 {
		log.Error("We cannot start a crawl without workers")
		return nil
	}

	log.Debug("Adding bootstraps")
	cm.toCrawl = append(cm.toCrawl, bootstraps...)
	log.Trace("Going into loop")

	infoTicker := time.NewTicker(20 * time.Second)
	defer infoTicker.Stop()
	prometheusTicker := time.NewTicker(time.Second)
	defer prometheusTicker.Stop()
	idleTimer := time.NewTimer(1 * time.Minute)
	defer idleTimer.Stop()
	for {
		// check if we can break the loop
		if len(cm.tokenBucket) == cm.queueSize &&
			len(cm.toCrawl) == 0 &&
			len(cm.ReportQueue) == 0 {
			log.Info("Stopping crawl...")
			break
		}
		idleTimer.Reset(1 * time.Minute)
		select {
		case report := <-cm.ReportQueue:
			// We have new information incomming
			node := report.Node
			err := report.Err
			// First, stop the idle timer. The following code is from the docs, apparently there are race conditions
			// with Stop() and the timer channel we're reading from.
			if !idleTimer.Stop() {
				<-idleTimer.C
			}
			if err != nil {
				log.WithFields(log.Fields{"Error": err}).Debug("Error while crawling")
				// TODO: Error handling
				continue
			} else {
				cm.online[node.id] = true
				cm.knows[node.id] = AddrInfoToID(node.knows)
				cm.info[node.id] = node.info // TODO: make the map merge together not overwrite each other
				// Notify prometheus about a new online node
				promMetricNumberOfNewIDs.WithLabelValues("reachable").Inc()
				for _, p := range node.knows {
					cm.handleInputNodes(p)
				}
				log.WithFields(log.Fields{
					"Current Request": cm.queueSize - len(cm.tokenBucket),
					"toCrawl":         len(cm.toCrawl),
					"Reports":         len(cm.ReportQueue),
				}).Debug("Status of Manager")
			}
		case id := <-cm.tokenBucket:
			// We can start a crawl, so let's do that
			if len(cm.toCrawl) > 0 {
				var node *peer.AddrInfo
				node, cm.toCrawl = cm.toCrawl[0], cm.toCrawl[1:]
				log.WithFields(log.Fields{"node": node.ID}).Debug("Dispatch crawler request")
				go cm.dispatch(node, id)
			} else {
				// nothing to do; return token
				cm.tokenBucket <- id
			}
		case <-infoTicker.C:
			log.WithFields(log.Fields{
				"Found nodes":          len(cm.crawled),
				"Waiting for requests": cm.queueSize - len(cm.tokenBucket),
				"To-crawl-queue":       len(cm.toCrawl),
				"Connectable nodes":    len(cm.online),
			}).Info("Periodic info on crawl status")

		case <-prometheusTicker.C:
			// Prometheus stats
			promMetricWaitingForRequests.Set(float64(cm.queueSize - len(cm.tokenBucket)))
			promMetricTokenBucketLength.Set(float64(len(cm.tokenBucket)))

		case <-idleTimer.C:
			// Stop the crawl
			log.Debug("Idle timer fired, stopping the crawl.")
			break
		}
	}

	return cm.createReport()
}

func (cm *CrawlManagerV2) dispatch(node *peer.AddrInfo, id int) {
	worker := *cm.workers[id]
	result, err := worker.CrawlPeer(node) // FIXME: worker selection
	if err != nil {
		// TODO: failed connection callback
	} else {
		// TODO: successful connection callback
	}
	cm.ReportQueue <- CrawlResult{Node: result, Err: err}
	cm.tokenBucket <- id
}

func (cm *CrawlManagerV2) handleInputNodes(node *peer.AddrInfo) {
	oldAddrs, crawled := cm.crawled[node.ID]
	_, online := cm.online[node.ID]
	if crawled && online {
		return
	}
	if crawled && !online {
		// Check if there are any new addresses. If so, connect to them
		newAddrs := FindNewMA(oldAddrs, stripLocalAddrs(*node).Addrs)
		if len(newAddrs) == 0 {
			// Nothing new, don't bother dialing again
			return
		}
		log.WithFields(log.Fields{"node": node.ID}).Debug("Adding new Addresses to crawled")
		cm.crawled[node.ID] = append(cm.crawled[node.ID], newAddrs...)
		workload := peer.AddrInfo{
			ID:    node.ID,
			Addrs: newAddrs,
		}
		log.WithFields(log.Fields{"node": node.ID}).Debug("Try new addresses")
		cm.toCrawl = append(cm.toCrawl, &workload)
		return
	}
	// If not, we remember that we've seen it and add it to the work queue, so that a worker will eventually crawl it.
	// Notify prometheus about newly learned peer
	promMetricNumberOfNewIDs.WithLabelValues("all").Inc()
	cm.crawled[node.ID] = node.Addrs
	log.WithFields(log.Fields{"node": node.ID}).Debug("Adding newer seen node")
	cm.toCrawl = append(cm.toCrawl, node)
}

func (cm *CrawlManagerV2) createReport() *CrawlOutput {
	// Output a crawl report into the log
	log.WithFields(log.Fields{
		"start time":        cm.startTime.Format(cm.config.Output.FilenameTimeFormat),
		"end time:":         time.Now().Format(cm.config.Output.FilenameTimeFormat),
		"number of nodes":   len(cm.crawled),
		"connectable nodes": len(cm.online),
	}).Info("Crawl finished. Summary of results.")

	out := CrawlOutput{StartDate: cm.startTime.Format(cm.config.Output.FilenameTimeFormat), EndDate: time.Now().Format(cm.config.Output.FilenameTimeFormat), Nodes: map[peer.ID]*CrawledNode{}}
	for node, Addresses := range cm.crawled {
		var status CrawledNode
		status.NID = node
		status.MultiAddrs = Addresses
		if online, found := cm.online[node]; found {
			status.Reachable = online
		} else {
			status.Reachable = false // Default value if not found
		}
		if neighbours, found := cm.knows[node]; found {
			status.Neighbours = neighbours
		} else {
			status.Neighbours = []peer.ID{}
		}
		if cm.info[node]["version"] != nil {
			status.AgentVersion = cm.info[node]["version"].(string)
		} else {
			status.AgentVersion = ""
		}
		if cm.info[node]["knows_timestamp"] != nil {
			status.Timestamp = cm.info[node]["knows_timestamp"].(string)
		} else {
			status.Timestamp = ""
		}

		out.Nodes[node] = &status
	}
	return &out
}

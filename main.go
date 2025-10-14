package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/c12s/hyparview/data"
	"github.com/c12s/hyparview/hyparview"
	"github.com/c12s/hyparview/transport"
	"github.com/caarlos0/env"
)

const FU_MSG_TYPE data.MessageType = data.UNKNOWN + 1

type FlowUpdate struct {
	NodeID   string
	Flow     float64
	Estimate float64
}

type Node struct {
	ID        string
	TAgg      int
	Value     float64
	Flows     map[string]float64
	Estimates map[string]float64
	Ticks     map[string]int
	Rcvd      map[hyparview.Peer]FlowUpdate
	Hyparview *hyparview.HyParView
	Lock      *sync.Mutex
	Logger    *log.Logger
}

func (n *Node) sumFlows() float64 {
	total := 0.0
	for _, f := range n.Flows {
		total += f
	}
	return total
}

func (n *Node) localEstimate() float64 {
	return n.Value - n.sumFlows()
}

func (n *Node) init() {
	n.Lock.Lock()
	defer n.Lock.Unlock()
	activePeers := n.Hyparview.GetPeers(10000)
	for _, peer := range activePeers {
		msg := FlowUpdate{
			NodeID:   n.ID,
			Flow:     0,
			Estimate: n.Value,
		}
		err := peer.Conn.Send(data.Message{
			Type:    FU_MSG_TYPE,
			Payload: msg,
		})
		if err != nil {
			n.Logger.Println(err)
		}
	}
}

func (n *Node) receive(msg FlowUpdate, peer hyparview.Peer) {
	n.Estimates[msg.NodeID] = msg.Estimate
	n.Flows[msg.NodeID] = -msg.Flow
	n.avgAndSend(peer)
}

func (n *Node) tick() {
	ticker := time.NewTicker(time.Duration(n.TAgg) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		n.Lock.Lock()
		activePeers := n.Hyparview.GetPeers(10000)
		for _, peer := range activePeers {
			n.Ticks[peer.Node.ID] = n.Ticks[peer.Node.ID] + 1
			if n.Ticks[peer.Node.ID] > 3 {
				n.avgAndSend(peer)
			}
		}
		n.Lock.Unlock()
	}
}

func (n *Node) avgAndSend(peer hyparview.Peer) {
	peerID := peer.Node.ID

	e := n.localEstimate()
	a := (n.Estimates[peerID] + e) / 2
	n.Flows[peerID] = n.Flows[peerID] + a - n.Estimates[peerID]
	n.Estimates[peerID] = a
	n.Ticks[peerID] = 0

	msg := FlowUpdate{
		NodeID:   n.ID,
		Flow:     n.Flows[peerID],
		Estimate: a,
	}
	err := peer.Conn.Send(data.Message{
		Type:    FU_MSG_TYPE,
		Payload: msg,
	})
	if err != nil {
		n.Logger.Println(err)
	}
}

func (n *Node) process() {
	ticker := time.NewTicker(time.Duration(n.TAgg) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		n.Lock.Lock()
		for peer, msg := range n.Rcvd {
			n.receive(msg, peer)
		}
		n.Logger.Printf("Current estimate %.2f\n", n.localEstimate())
		n.Logger.Printf("Sent %d\n", transport.MessagesSent)
		n.Logger.Printf("Rcvd %d\n", transport.MessagesRcvd)
		n.Lock.Unlock()
	}
}

func (n *Node) setMetricsHandler(w http.ResponseWriter, r *http.Request) {
	newMetrics, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()
	lines := strings.Split(string(newMetrics), "\n")
	valStr := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "app_memory_usage_bytes") {
			valStr = strings.Split(line, " ")[1]
			break
		}
	}
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		n.Logger.Println(err)
	} else {
		n.Logger.Println("new value", val)
		n.Value = val
	}
	w.WriteHeader(http.StatusOK)
}

func main() {
	hvConfig := hyparview.Config{}
	err := env.Parse(&hvConfig)
	if err != nil {
		log.Fatal(err)
	}

	cfg := Config{}
	err = env.Parse(&cfg)
	if err != nil {
		log.Fatal(err)
	}

	self := data.Node{
		ID:            cfg.NodeID,
		ListenAddress: cfg.ListenAddr,
	}

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lshortfile)

	gnConnManager := transport.NewConnManager(
		transport.NewTCPConn,
		transport.AcceptTcpConnsFn(self.ListenAddress),
	)

	hv, err := hyparview.NewHyParView(hvConfig, self, gnConnManager, logger)
	if err != nil {
		log.Fatal(err)
	}

	tAgg, err := strconv.Atoi(cfg.TAgg)
	if err != nil {
		logger.Fatal(err)
	}

	val, err := strconv.Atoi(strings.Split(cfg.NodeID, "_")[2])
	if err != nil {
		logger.Fatal(err)
	}

	node := &Node{
		ID:        cfg.NodeID,
		TAgg:      tAgg,
		Value:     float64(val),
		Flows:     make(map[string]float64),
		Estimates: make(map[string]float64),
		Ticks:     make(map[string]int),
		Rcvd:      make(map[hyparview.Peer]FlowUpdate),
		Hyparview: hv,
		Lock:      &sync.Mutex{},
		Logger:    logger,
	}

	hv.AddClientMsgHandler(FU_MSG_TYPE, func(msgBytes []byte, sender hyparview.Peer) {
		msg := FlowUpdate{}
		err := transport.Deserialize(msgBytes, &msg)
		if err != nil {
			logger.Println(node.ID, "-", "Error unmarshaling message:", err)
			return
		}
		node.Lock.Lock()
		defer node.Lock.Unlock()
		node.Rcvd[sender] = msg
	})
	hv.OnPeerDown(func(peer hyparview.Peer) {
		node.Lock.Lock()
		defer node.Lock.Unlock()
		delete(node.Estimates, peer.Node.ID)
		delete(node.Flows, peer.Node.ID)
		delete(node.Ticks, peer.Node.ID)
		delete(node.Rcvd, peer)
	})

	go func() {
		for range time.NewTicker(time.Second).C {
			node.exportMsgCount()
			node.Lock.Lock()
			value := node.localEstimate()
			node.Lock.Unlock()
			node.exportResult(value, 0, time.Now().UnixNano())
		}
	}()

	err = hv.Join(cfg.ContactID, cfg.ContactAddr)
	if err != nil {
		logger.Fatal(err)
	}

	go node.process()
	go node.tick()
	node.init()

	r := http.NewServeMux()
	r.HandleFunc("POST /metrics", node.setMetricsHandler)
	log.Println("Metrics server listening on :9200/metrics")

	go func() {
		log.Fatal(http.ListenAndServe(strings.Split(os.Getenv("LISTEN_ADDR"), ":")[0]+":9200", r))
	}()

	r2 := http.NewServeMux()
	r2.HandleFunc("GET /state", node.StateHandler)
	log.Println("State server listening on :5001/state")
	log.Fatal(http.ListenAndServe(strings.Split(os.Getenv("LISTEN_ADDR"), ":")[0]+":5001", r2))
}

var writers map[string]*csv.Writer = map[string]*csv.Writer{}

func (n *Node) exportResult(value float64, reqTimestamp, rcvTimestamp int64) {
	name := "value"
	filename := fmt.Sprintf("/var/log/monoceros/results/%s.csv", name)
	// defer file.Close()
	writer := writers[filename]
	if writer == nil {
		file, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			n.Logger.Printf("failed to open/create file: %v", err)
			return
		}
		writer = csv.NewWriter(file)
		writers[filename] = writer
	}
	defer writer.Flush()
	reqTsStr := strconv.Itoa(int(reqTimestamp))
	rcvTsStr := strconv.Itoa(int(rcvTimestamp))
	valStr := strconv.FormatFloat(value, 'f', -1, 64)
	err := writer.Write([]string{"x", reqTsStr, rcvTsStr, valStr})
	if err != nil {
		n.Logger.Println(err)
	}
}

func (n *Node) exportMsgCount() {
	filename := "/var/log/monoceros/results/msg_count.csv"
	// defer file.Close()
	writer := writers[filename]
	if writer == nil {
		file, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			n.Logger.Printf("failed to open/create file: %v", err)
			return
		}
		writer = csv.NewWriter(file)
		writers[filename] = writer
	}
	defer writer.Flush()
	tsStr := strconv.Itoa(int(time.Now().UnixNano()))
	transport.MessagesSentLock.Lock()
	sent := transport.MessagesSent - transport.MessagesSentSub
	transport.MessagesSentLock.Unlock()
	transport.MessagesRcvdLock.Lock()
	rcvd := transport.MessagesRcvd - transport.MessagesRcvdSub
	transport.MessagesRcvdLock.Unlock()
	sentStr := strconv.Itoa(sent)
	rcvdStr := strconv.Itoa(rcvd)
	err := writer.Write([]string{tsStr, sentStr, rcvdStr})
	if err != nil {
		n.Logger.Println(err)
	}
}

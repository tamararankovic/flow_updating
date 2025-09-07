package main

import (
	"log"
	"math"
	"os"
	"slices"
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
	Rcvd      []string
	Ticks     int
	Msgs      map[hyparview.Peer][]FlowUpdate
	Hyparview *hyparview.HyParView
	Lock      *sync.Mutex
	Logger    *log.Logger
	Expected  float64
}

func (n *Node) localEstimate() float64 {
	return n.Value - sum(n.Flows)
}

func (n *Node) rcvdAll() bool {
	for _, peer := range n.Hyparview.GetPeers(1000) {
		if !slices.Contains(n.Rcvd, peer.Node.ID) {
			return false
		}
	}
	return true
}

func (n *Node) init() {
	n.Lock.Lock()
	defer n.Lock.Unlock()
	activePeers := n.Hyparview.GetPeers(10000)
	if len(activePeers) == 0 {
		n.Logger.Println("no peers")
		return
	}
	for _, peer := range activePeers {
		n.Msgs[peer] = append(n.Msgs[peer], FlowUpdate{
			NodeID:   n.ID,
			Flow:     0,
			Estimate: n.Value,
		})
	}
}

func (n *Node) receive(msg FlowUpdate) {
	n.Lock.Lock()
	defer n.Lock.Unlock()

	n.Estimates[msg.NodeID] = msg.Estimate
	n.Flows[msg.NodeID] = -msg.Flow
	n.Rcvd = append(n.Rcvd, msg.NodeID)
	if n.rcvdAll() {
		n.avgAndSend()
	}
}

func (n *Node) tick() {
	ticker := time.NewTicker(time.Duration(n.TAgg) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		n.Lock.Lock()
		n.Ticks += 1
		if n.Ticks > 3 {
			n.avgAndSend()
		}
		n.Lock.Unlock()
	}
}

func (n *Node) avgAndSend() {
	peers := n.Hyparview.GetPeers(1000)

	e := n.localEstimate()
	a := (e + sum(n.Estimates)) / (float64(len(peers)) + 1)
	for _, peer := range peers {
		peerID := peer.Node.ID
		n.Flows[peerID] = n.Flows[peerID] + a - n.Estimates[peerID]
		n.Estimates[peerID] = a
		n.Msgs[peer] = append(n.Msgs[peer], FlowUpdate{
			NodeID:   n.ID,
			Flow:     n.Flows[peerID],
			Estimate: a,
		})
	}
	n.Rcvd = make([]string, 0)
	n.Ticks = 0
}

func (n *Node) sendMsgs() {
	ticker := time.NewTicker(time.Duration(n.TAgg) * time.Second)
	defer ticker.Stop()

	rounds := 0

	for range ticker.C {
		n.Lock.Lock()
		if !equal(n.localEstimate(), n.Expected, n.Expected/100) {
			rounds++
		}
		n.Logger.Printf("Expected estimate %.2f\n", n.Expected)
		n.Logger.Printf("Current estimate %.2f\n", n.localEstimate())
		n.Logger.Printf("Rounds to converge %d\n", rounds)

		for peer, msgs := range n.Msgs {
			for _, msg := range msgs {
				err := peer.Conn.Send(data.Message{
					Type:    FU_MSG_TYPE,
					Payload: msg,
				})
				if err != nil {
					n.Logger.Println(err)
				}
			}
		}
		n.Msgs = make(map[hyparview.Peer][]FlowUpdate)
		n.Lock.Unlock()
	}
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

	expected, err := strconv.ParseFloat(cfg.Expected, 64)
	if err != nil {
		logger.Fatal(err)
	}

	node := &Node{
		ID:        cfg.NodeID,
		TAgg:      tAgg,
		Value:     float64(val),
		Flows:     make(map[string]float64),
		Estimates: make(map[string]float64),
		Rcvd:      make([]string, 0),
		Msgs:      make(map[hyparview.Peer][]FlowUpdate),
		Hyparview: hv,
		Lock:      &sync.Mutex{},
		Logger:    logger,
		Expected:  expected,
	}

	hv.AddClientMsgHandler(FU_MSG_TYPE, func(msgBytes []byte, sender hyparview.Peer) {
		msg := FlowUpdate{}
		err := transport.Deserialize(msgBytes, &msg)
		if err != nil {
			logger.Println(node.ID, "-", "Error unmarshaling message:", err)
			return
		}
		node.receive(msg)
	})
	hv.OnPeerDown(func(peer hyparview.Peer) {
		node.Lock.Lock()
		defer node.Lock.Unlock()
		delete(node.Estimates, peer.Node.ID)
		delete(node.Flows, peer.Node.ID)
		delete(node.Msgs, peer)
	})

	err = hv.Join(cfg.ContactID, cfg.ContactAddr)
	if err != nil {
		logger.Fatal(err)
	}
	time.Sleep(20 * time.Second)

	go node.sendMsgs()
	go node.tick()
	node.init()

	select {}
}

func sum(m map[string]float64) float64 {
	total := 0.0
	for _, f := range m {
		total += f
	}
	return total
}

func equal(a, b, epsilon float64) bool {
	return math.Abs(a-b) < epsilon
}

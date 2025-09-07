package main

// import (
// 	"log"
// 	"os"
// 	"strconv"
// 	"strings"
// 	"sync"
// 	"time"

// 	"github.com/c12s/hyparview/data"
// 	"github.com/c12s/hyparview/hyparview"
// 	"github.com/c12s/hyparview/transport"
// 	"github.com/caarlos0/env"
// )

// const FU_MSG_TYPE data.MessageType = data.UNKNOWN + 1

// type FlowUpdate struct {
// 	NodeID   string
// 	Flow     float64
// 	Estimate float64
// }

// type Node struct {
// 	ID        string
// 	TAgg      int
// 	Value     float64
// 	Flows     map[string]float64
// 	Estimates map[string]float64
// 	Ticks     map[string]int
// 	Msgs      map[hyparview.Peer][]FlowUpdate
// 	Hyparview *hyparview.HyParView
// 	Lock      *sync.Mutex
// 	Logger    *log.Logger
// }

// func (n *Node) sumFlows() float64 {
// 	total := 0.0
// 	for _, f := range n.Flows {
// 		total += f
// 	}
// 	return total
// }

// func (n *Node) localEstimate() float64 {
// 	return n.Value - n.sumFlows()
// }

// func (n *Node) init() {
// 	n.Lock.Lock()
// 	defer n.Lock.Unlock()
// 	activePeers := n.Hyparview.GetPeers(10000)
// 	if len(activePeers) == 0 {
// 		n.Logger.Println("no peers")
// 		return
// 	}
// 	for _, peer := range activePeers {
// 		n.Msgs[peer] = append(n.Msgs[peer], FlowUpdate{
// 			NodeID:   n.ID,
// 			Flow:     0,
// 			Estimate: n.Value,
// 		})
// 	}
// }

// func (n *Node) receive(msg FlowUpdate, peer hyparview.Peer) {
// 	n.Lock.Lock()
// 	defer n.Lock.Unlock()

// 	n.Estimates[msg.NodeID] = msg.Estimate
// 	n.Flows[msg.NodeID] = -msg.Flow
// 	n.avgAndSend(peer)
// }

// func (n *Node) tick() {
// 	ticker := time.NewTicker(time.Duration(n.TAgg) * time.Second)
// 	defer ticker.Stop()

// 	for range ticker.C {
// 		n.Lock.Lock()
// 		activePeers := n.Hyparview.GetPeers(10000)
// 		if len(activePeers) == 0 {
// 			return
// 		}
// 		for _, peer := range activePeers {
// 			n.Ticks[peer.Node.ID] = n.Ticks[peer.Node.ID] + 1
// 			if n.Ticks[peer.Node.ID] > 3 {
// 				n.avgAndSend(peer)
// 			}
// 		}
// 		n.Lock.Unlock()
// 	}
// }

// func (n *Node) avgAndSend(peer hyparview.Peer) {
// 	peerID := peer.Node.ID

// 	e := n.localEstimate()
// 	a := (n.Estimates[peerID] + e) / 2
// 	n.Flows[peerID] = n.Flows[peerID] + a - n.Estimates[peerID]
// 	n.Estimates[peerID] = a
// 	n.Ticks[peerID] = 0

// 	n.Msgs[peer] = append(n.Msgs[peer], FlowUpdate{
// 		NodeID:   n.ID,
// 		Flow:     n.Flows[peerID],
// 		Estimate: a,
// 	})
// }

// func (n *Node) sendMsgs() {
// 	ticker := time.NewTicker(time.Duration(n.TAgg) * time.Second)
// 	defer ticker.Stop()

// 	for range ticker.C {
// 		n.Lock.Lock()
// 		n.Logger.Printf("Current estimate %.2f\n", n.localEstimate())
// 		for peer, msgs := range n.Msgs {
// 			for _, msg := range msgs {
// 				err := peer.Conn.Send(data.Message{
// 					Type:    FU_MSG_TYPE,
// 					Payload: msg,
// 				})
// 				if err != nil {
// 					n.Logger.Println(err)
// 				}
// 			}
// 		}
// 		n.Msgs = make(map[hyparview.Peer][]FlowUpdate)
// 		n.Lock.Unlock()
// 	}
// }

// func main() {
// 	hvConfig := hyparview.Config{}
// 	err := env.Parse(&hvConfig)
// 	if err != nil {
// 		log.Fatal(err)
// 	}

// 	cfg := Config{}
// 	err = env.Parse(&cfg)
// 	if err != nil {
// 		log.Fatal(err)
// 	}

// 	self := data.Node{
// 		ID:            cfg.NodeID,
// 		ListenAddress: cfg.ListenAddr,
// 	}

// 	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lshortfile)

// 	gnConnManager := transport.NewConnManager(
// 		transport.NewTCPConn,
// 		transport.AcceptTcpConnsFn(self.ListenAddress),
// 	)

// 	hv, err := hyparview.NewHyParView(hvConfig, self, gnConnManager, logger)
// 	if err != nil {
// 		log.Fatal(err)
// 	}

// 	tAgg, err := strconv.Atoi(cfg.TAgg)
// 	if err != nil {
// 		logger.Fatal(err)
// 	}

// 	val, err := strconv.Atoi(strings.Split(cfg.NodeID, "_")[2])
// 	if err != nil {
// 		logger.Fatal(err)
// 	}

// 	node := &Node{
// 		ID:        cfg.NodeID,
// 		TAgg:      tAgg,
// 		Value:     float64(val),
// 		Flows:     make(map[string]float64),
// 		Estimates: make(map[string]float64),
// 		Ticks:     make(map[string]int),
// 		Msgs:      make(map[hyparview.Peer][]FlowUpdate),
// 		Hyparview: hv,
// 		Lock:      &sync.Mutex{},
// 		Logger:    logger,
// 	}

// 	hv.AddClientMsgHandler(FU_MSG_TYPE, func(msgBytes []byte, sender hyparview.Peer) {
// 		msg := FlowUpdate{}
// 		err := transport.Deserialize(msgBytes, &msg)
// 		if err != nil {
// 			logger.Println(node.ID, "-", "Error unmarshaling message:", err)
// 			return
// 		}
// 		node.receive(msg, sender)
// 	})
// 	hv.OnPeerDown(func(peer hyparview.Peer) {
// 		node.Lock.Lock()
// 		defer node.Lock.Unlock()
// 		delete(node.Estimates, peer.Node.ID)
// 		delete(node.Flows, peer.Node.ID)
// 		delete(node.Ticks, peer.Node.ID)
// 		delete(node.Msgs, peer)
// 	})

// 	err = hv.Join(cfg.ContactID, cfg.ContactAddr)
// 	if err != nil {
// 		logger.Fatal(err)
// 	}
// 	time.Sleep(60 * time.Second)

// 	go node.sendMsgs()
// 	go node.tick()
// 	node.init()

// 	select {}
// }

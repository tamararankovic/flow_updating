package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tamararankovic/flow_updating/config"
	"github.com/tamararankovic/flow_updating/peers"
)

const FU_MSG_TYPE int8 = 1

type Msg interface {
	Type() int8
}

type FlowUpdate struct {
	NodeID   string
	Flow     float64
	Estimate float64
}

func (m FlowUpdate) Type() int8 {
	return FU_MSG_TYPE
}

func MsgToBytes(msg Msg) []byte {
	msgBytes, _ := json.Marshal(&msg)
	return append([]byte{byte(msg.Type())}, msgBytes...)
}

func BytesToMsg(msgBytes []byte) Msg {
	msgType := int8(msgBytes[0])
	var msg Msg
	switch msgType {
	case FU_MSG_TYPE:
		msg = &FlowUpdate{}
	}
	if msg == nil {
		return nil
	}
	json.Unmarshal(msgBytes[1:], msg)
	return msg
}

type Node struct {
	ID        string
	TAgg      int
	Value     float64
	Flows     map[string]float64
	Estimates map[string]float64
	Ticks     map[string]int
	Rcvd      map[peers.Peer]*FlowUpdate
	Peers     *peers.Peers
	Lock      *sync.Mutex
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
	activePeers := n.Peers.GetPeers()
	for _, peer := range activePeers {
		msg := FlowUpdate{
			NodeID:   n.ID,
			Flow:     0,
			Estimate: n.Value,
		}
		peer.Send(MsgToBytes(msg))
	}
}

func (n *Node) receive(msg FlowUpdate, peer peers.Peer) {
	n.Estimates[msg.NodeID] = msg.Estimate
	n.Flows[msg.NodeID] = -msg.Flow
	n.avgAndSend(peer)
}

func (n *Node) tick() {
	ticker := time.NewTicker(time.Duration(n.TAgg) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		n.Lock.Lock()
		activePeers := n.Peers.GetPeers()
		for _, peer := range activePeers {
			n.Ticks[peer.GetID()] = n.Ticks[peer.GetID()] + 1
			if n.Ticks[peer.GetID()] > 3 {
				n.avgAndSend(peer)
			}
		}
		n.Lock.Unlock()
	}
}

func (n *Node) avgAndSend(peer peers.Peer) {
	peerID := peer.GetID()

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
	peer.Send(MsgToBytes(msg))
}

func (n *Node) process() {
	ticker := time.NewTicker(time.Duration(n.TAgg) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		n.Lock.Lock()
		for peer, msg := range n.Rcvd {
			n.receive(*msg, peer)
		}
		log.Printf("Current estimate %.2f\n", n.localEstimate())
		log.Printf("Sent %d\n", peers.MessagesSent)
		log.Printf("Rcvd %d\n", peers.MessagesRcvd)
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
		log.Println(err)
	} else {
		log.Println("new value", val)
		n.Value = val
	}
	w.WriteHeader(http.StatusOK)
}

func main() {
	time.Sleep(10 * time.Second)

	cfg := config.LoadConfigFromEnv()
	params := config.LoadParamsFromEnv()

	ps, err := peers.NewPeers(cfg)
	if err != nil {
		log.Fatal(err)
	}

	val, err := strconv.Atoi(params.ID)
	if err != nil {
		log.Fatal(err)
	}

	node := &Node{
		ID:        params.ID,
		TAgg:      params.Tagg,
		Value:     float64(val),
		Flows:     make(map[string]float64),
		Estimates: make(map[string]float64),
		Ticks:     make(map[string]int),
		Rcvd:      make(map[peers.Peer]*FlowUpdate),
		Peers:     ps,
		Lock:      &sync.Mutex{},
	}

	lastRcvd := make(map[string]int)
	round := 0

	// handle messages
	go func() {
		for msgRcvd := range ps.Messages {
			msg := BytesToMsg(msgRcvd.MsgBytes)
			if msg == nil {
				continue
			}
			lastRcvd[msgRcvd.Sender.GetID()] = round
			node.Lock.Lock()
			node.Rcvd[msgRcvd.Sender] = msg.(*FlowUpdate)
			node.Lock.Unlock()
		}
	}()

	// remove failed peers
	go func() {
		for range time.NewTicker(time.Second).C {
			round++
			for _, peer := range ps.GetPeers() {
				if lastRcvd[peer.GetID()]+params.Rmax < round && round > 10 {
					ps.PeerFailed(peer.GetID())
					node.Lock.Lock()
					delete(node.Estimates, peer.GetID())
					delete(node.Flows, peer.GetID())
					delete(node.Ticks, peer.GetID())
					delete(node.Rcvd, peer)
					node.Lock.Unlock()
				}
			}
		}
	}()

	go func() {
		for range time.NewTicker(time.Second).C {
			node.exportMsgCount()
			node.Lock.Lock()
			value := node.localEstimate()
			node.Lock.Unlock()
			node.exportResult(value, 0, time.Now().UnixNano())
		}
	}()

	go node.process()
	go node.tick()
	node.init()

	r := http.NewServeMux()
	r.HandleFunc("POST /metrics", node.setMetricsHandler)
	log.Println("Metrics server listening on :9200/metrics")

	log.Fatal(http.ListenAndServe(strings.Split(os.Getenv("LISTEN_ADDR"), ":")[0]+":9200", r))
}

var writers map[string]*csv.Writer = map[string]*csv.Writer{}

func (n *Node) exportResult(value float64, reqTimestamp, rcvTimestamp int64) {
	name := "value"
	filename := fmt.Sprintf("/var/log/flow_updating/%s.csv", name)
	writer := writers[filename]
	if writer == nil {
		file, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			log.Printf("failed to open/create file: %v", err)
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
		log.Println(err)
	}
}

func (n *Node) exportMsgCount() {
	filename := "/var/log/flow_updating/msg_count.csv"
	writer := writers[filename]
	if writer == nil {
		file, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			log.Printf("failed to open/create file: %v", err)
			return
		}
		writer = csv.NewWriter(file)
		writers[filename] = writer
	}
	defer writer.Flush()
	tsStr := strconv.Itoa(int(time.Now().UnixNano()))
	peers.MessagesSentLock.Lock()
	sent := peers.MessagesSent
	peers.MessagesSentLock.Unlock()
	peers.MessagesRcvdLock.Lock()
	rcvd := peers.MessagesRcvd
	peers.MessagesRcvdLock.Unlock()
	sentStr := strconv.Itoa(sent)
	rcvdStr := strconv.Itoa(rcvd)
	err := writer.Write([]string{tsStr, sentStr, rcvdStr})
	if err != nil {
		log.Println(err)
	}
}

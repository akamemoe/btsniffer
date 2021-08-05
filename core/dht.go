package core

import (
	"bytes"
	"container/list"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/marksamman/bencode"
	"golang.org/x/time/rate"
)

var seeds = []string{
	"router.bittorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"router.utorrent.com:6881",
}

type nodeID []byte

type Node struct {
	Addr string
	Id   string
}

type Announcements struct {
	Mu    sync.RWMutex
	Ll    *list.List
	Limit int
	Input chan struct{}
}

func (a *Announcements) Get() *Announcement {
	a.Mu.Lock()
	defer a.Mu.Unlock()

	if elem := a.Ll.Front(); elem != nil {
		ac := elem.Value.(*Announcement)
		a.Ll.Remove(elem)
		return ac
	}

	return nil
}

func (a *Announcements) Put(ac *Announcement) {
	a.Mu.Lock()
	defer a.Mu.Unlock()

	if a.Ll.Len() >= a.Limit {
		return
	}

	a.Ll.PushBack(ac)

	select {
	case a.Input <- struct{}{}:
	default:
	}
}

func (a *Announcements) Wait() <-chan struct{} {
	return a.Input
}

func (a *Announcements) Len() int {
	a.Mu.RLock()
	defer a.Mu.RUnlock()

	return a.Ll.Len()
}

func (a *Announcements) Full() bool {
	return a.Len() >= a.Limit
}

type Announcement struct {
	Raw         map[string]interface{}
	From        net.UDPAddr
	Peer        net.Addr
	InfoHash    []byte
	InfoHashHex string
}

func RandBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func neighborID(target nodeID, local nodeID) nodeID {
	const closeness = 15
	id := make([]byte, 20)
	copy(id[:closeness], target[:closeness])
	copy(id[closeness:], local[closeness:])
	return id
}

func makeQuery(tid string, q string, a map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"t": tid,
		"y": "q",
		"q": q,
		"a": a,
	}
}

func makeReply(tid string, r map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"t": tid,
		"y": "r",
		"r": r,
	}
}

func decodeNodes(s string) (nodes []*Node) {
	length := len(s)
	if length%26 != 0 {
		return
	}

	for i := 0; i < length; i += 26 {
		id := s[i : i+20]
		ip := net.IP([]byte(s[i+20 : i+24])).String()
		port := binary.BigEndian.Uint16([]byte(s[i+24 : i+26]))
		addr := ip + ":" + strconv.Itoa(int(port))
		nodes = append(nodes, &Node{Id: id, Addr: addr})
	}

	return
}

func per(events int, duration time.Duration) rate.Limit {
	return rate.Every(duration / time.Duration(events))
}

type Dht struct {
	Mu             sync.Mutex
	Announcements  *Announcements
	ChNode         chan *Node
	Die            chan struct{}
	ErrDie         error
	LocalID        nodeID
	Conn           *net.UDPConn
	QueryTypes     map[string]func(map[string]interface{}, net.UDPAddr)
	FriendsLimiter *rate.Limiter
	Secret         []byte
	Seeds          []string
}

func NewDHT(laddr string, maxFriendsPerSec int) (*Dht, error) {
	conn, err := net.ListenPacket("udp", laddr)
	if err != nil {
		return nil, err
	}

	d := &Dht{
		Announcements: &Announcements{
			Ll:    list.New(),
			Limit: maxFriendsPerSec * 10,
			Input: make(chan struct{}, 1),
		},
		LocalID: RandBytes(20),
		Conn:    conn.(*net.UDPConn),
		ChNode:  make(chan *Node),
		Die:     make(chan struct{}),
		Secret:  RandBytes(20),
	}
	d.FriendsLimiter = rate.NewLimiter(per(maxFriendsPerSec, time.Second), maxFriendsPerSec)
	d.QueryTypes = map[string]func(map[string]interface{}, net.UDPAddr){
		"get_peers":     d.onGetPeersQuery,
		"announce_peer": d.onAnnouncePeerQuery,
	}
	return d, nil
}

func (d *Dht) Run() {
	go d.Listen()
	go d.Join()
	go d.MakeFriends()
}

func (d *Dht) Listen() {
	buf := make([]byte, 2048)
	for {
		n, addr, err := d.Conn.ReadFromUDP(buf)
		if err == nil {
			d.onMessage(buf[:n], *addr)
		} else {
			d.ErrDie = err
			close(d.Die)
			break
		}
	}
}

func (d *Dht) Join() {
	const timesForSure = 3
	for i := 0; i < timesForSure; i++ {
		for _, addr := range seeds {
			select {
			case d.ChNode <- &Node{
				Addr: addr,
				Id:   string(RandBytes(20)),
			}:
			case <-d.Die:
				return
			}
		}
	}
}

func (d *Dht) onMessage(data []byte, from net.UDPAddr) {
	dict, err := bencode.Decode(bytes.NewBuffer(data))
	if err != nil {
		return
	}

	y, ok := dict["y"].(string)
	if !ok {
		return
	}

	switch y {
	case "q":
		d.onQuery(dict, from)
	case "r", "e":
		d.onReply(dict, from)
	}
}

func (d *Dht) onQuery(dict map[string]interface{}, from net.UDPAddr) {
	_, ok := dict["t"].(string)
	if !ok {
		return
	}

	q, ok := dict["q"].(string)
	if !ok {
		return
	}

	if handle, ok := d.QueryTypes[q]; ok {
		handle(dict, from)
	}
}

func (d *Dht) onReply(dict map[string]interface{}, from net.UDPAddr) {
	r, ok := dict["r"].(map[string]interface{})
	if !ok {
		return
	}

	nodes, ok := r["nodes"].(string)
	if !ok {
		return
	}

	for _, node := range decodeNodes(nodes) {
		if !d.FriendsLimiter.Allow() {
			continue
		}

		d.ChNode <- node
	}
}

func (d *Dht) FindNode(to string, target nodeID) {
	q := makeQuery(string(RandBytes(2)), "find_node", map[string]interface{}{
		"id":     string(neighborID(target, d.LocalID)),
		"target": string(RandBytes(20)),
	})

	addr, err := net.ResolveUDPAddr("udp", to)
	if err != nil {
		return
	}

	d.Send(q, *addr)
}

func (d *Dht) onGetPeersQuery(dict map[string]interface{}, from net.UDPAddr) {
	tid, ok := dict["t"].(string)
	if !ok {
		return
	}

	a, ok := dict["a"].(map[string]interface{})
	if !ok {
		return
	}

	id, ok := a["id"].(string)
	if !ok {
		return
	}

	r := makeReply(tid, map[string]interface{}{
		"id":    string(neighborID([]byte(id), d.LocalID)),
		"nodes": "",
		"token": d.makeToken(from),
	})
	d.Send(r, from)
}

func (d *Dht) onAnnouncePeerQuery(dict map[string]interface{}, from net.UDPAddr) {
	if d.Announcements.Full() {
		return
	}

	a, ok := dict["a"].(map[string]interface{})
	if !ok {
		return
	}

	token, ok := a["token"].(string)
	if !ok || !d.validateToken(token, from) {
		return
	}

	if ac := d.Summarize(dict, from); ac != nil {
		d.Announcements.Put(ac)
	}
}

func (d *Dht) Summarize(dict map[string]interface{}, from net.UDPAddr) *Announcement {
	a, ok := dict["a"].(map[string]interface{})
	if !ok {
		return nil
	}

	infohash, ok := a["info_hash"].(string)
	if !ok {
		return nil
	}

	port := int64(from.Port)
	if impliedPort, ok := a["implied_port"].(int64); ok && impliedPort == 0 {
		if p, ok := a["port"].(int64); ok {
			port = p
		}
	}

	return &Announcement{
		Raw:         dict,
		From:        from,
		InfoHash:    []byte(infohash),
		InfoHashHex: hex.EncodeToString([]byte(infohash)),
		Peer:        &net.TCPAddr{IP: from.IP, Port: int(port)},
	}
}

func (d *Dht) Send(dict map[string]interface{}, to net.UDPAddr) error {
	d.Mu.Lock()
	defer d.Mu.Unlock()

	d.Conn.WriteToUDP(bencode.Encode(dict), &to)
	return nil
}

func (d *Dht) MakeFriends() {
	for {
		select {
		case node := <-d.ChNode:
			d.FindNode(node.Addr, []byte(node.Id))
		case <-d.Die:
			return
		}
	}
}

func (d *Dht) makeToken(from net.UDPAddr) string {
	s := sha1.New()
	s.Write([]byte(from.String()))
	s.Write(d.Secret)
	return string(s.Sum(nil))
}

func (d *Dht) validateToken(token string, from net.UDPAddr) bool {
	return token == d.makeToken(from)
}

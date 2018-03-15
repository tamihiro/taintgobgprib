package main

import (
	"container/heap"
	"github.com/osrg/gobgp/api"
	"github.com/osrg/gobgp/packet/bgp"
	"github.com/osrg/gobgp/table"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"log"
	"net"
	"os"
	"runtime"
	"time"
)

const IntervalMicrosec = 40
const RibSearcherNum = 300
const RibSearcherReqNum = 100
const GrpcTimeout = 1e2 * time.Millisecond
const SearchIpNum = 2

type SearchRequest struct {
	ReqCount   int
	SearchIp   []net.IP
	MatchedRib chan []*table.Destination
}

func (sr *SearchRequest) handleResult() {
	dsts := <-sr.MatchedRib
	m := make(map[*net.IP]bool)
	for i, _ := range sr.SearchIp {
		m[&sr.SearchIp[i]] = false
	}
	var n []*net.IPNet
	for _, d := range dsts {
		if d == nil {
			continue
		}
		_, v, err := net.ParseCIDR(d.GetNlri().String())
		if err != nil {
			log.Printf("ERROR: %v", err)
			continue
		}
		n = append(n, v)
		for i, _ := range sr.SearchIp {
			if v.Contains(sr.SearchIp[i]) {
				m[&sr.SearchIp[i]] = true
			}
		}
	}
	for _, b := range m {
		if !b {
			log.Printf("ERROR: search request #%v %v, result %v", sr.ReqCount, sr.SearchIp, n)
			os.Exit(1)
		}
	}

}

type RibSearcher struct {
	i          int
	requests   chan SearchRequest
	pending    int
	grpcClient gobgpapi.GobgpApiClient
}

func (rs *RibSearcher) lookupRib(ipAddrs ...net.IP) (ret []*table.Destination, err error) {
	lookupPrefix := make([]*table.LookupPrefix, 0, len(ipAddrs))
	for _, ip := range ipAddrs {
		lookupPrefix = append(lookupPrefix, &table.LookupPrefix{ip.String(), table.LOOKUP_EXACT})
	}
	dsts := make([]*gobgpapi.Destination, 0, len(lookupPrefix))
	for _, p := range lookupPrefix {
		dsts = append(dsts, &gobgpapi.Destination{
			Prefix:          p.Prefix,
			LongerPrefixes:  false,
			ShorterPrefixes: false,
		})
	}
	grpcRes, err := rs.grpcClient.GetRib(context.Background(), &gobgpapi.GetRibRequest{
		Table: &gobgpapi.Table{
			Type:         gobgpapi.Resource_GLOBAL,
			Family:       uint32(bgp.RF_IPv4_UC),
			Name:         "",
			Destinations: dsts,
		},
	})
	if err != nil {
		return
	}
	rib, err := grpcRes.Table.ToNativeTable()
	if err != nil {
		return
	}
	for _, d := range rib.GetDestinations() {
		ret = append(ret, d)
	}
	return
}

func (rs *RibSearcher) setGrpcClient() error {
	conn, err := grpc.DialContext(context.Background(), ":50051", grpc.WithTimeout(GrpcTimeout), grpc.WithBlock(), grpc.WithInsecure())
	if err != nil {
		return err
	}
	rs.grpcClient = gobgpapi.NewGobgpApiClient(conn)
	return nil
}

func (rs *RibSearcher) searchRib(done chan *RibSearcher) {
	for {
		sr := <-rs.requests
		go sr.handleResult()
		d, err := rs.lookupRib(sr.SearchIp...)
		if err != nil {
			log.Printf("ERROR: %v", err)
		}
		sr.MatchedRib <- d
		done <- rs
	}
}

type RibSearcherPool []*RibSearcher

func (p RibSearcherPool) Len() int { return len(p) }

func (p RibSearcherPool) Less(i, j int) bool {
	return p[i].pending < p[j].pending
}

func (p *RibSearcherPool) Swap(i, j int) {
	a := *p
	a[i], a[j] = a[j], a[i]
	a[i].i = i
	a[j].i = j
}

func (p *RibSearcherPool) Push(x interface{}) {
	a := *p
	n := len(a)
	a = a[0 : n+1]
	rs := x.(*RibSearcher)
	a[n] = rs
	rs.i = n
	*p = a
}

func (p *RibSearcherPool) Pop() interface{} {
	a := *p
	*p = a[0 : len(a)-1]
	rs := a[len(a)-1]
	rs.i = -1
	return rs
}

type RsBalancer struct {
	pool RibSearcherPool
	done chan *RibSearcher
	sr   chan *SearchRequest
	i    int
}

func NewRsBalancer() *RsBalancer {
	done := make(chan *RibSearcher, RibSearcherNum)
	sr := make(chan *SearchRequest)
	b := &RsBalancer{make(RibSearcherPool, 0, RibSearcherNum), done, sr, 0}
	for i := 0; i < RibSearcherNum; i++ {
		rs := &RibSearcher{requests: make(chan SearchRequest, RibSearcherReqNum)}
		if err := rs.setGrpcClient(); err != nil {
			log.Printf("ERROR: faied to create grpc client: %v", err)
		}
		heap.Push(&b.pool, rs)
		go rs.searchRib(b.done)
	}
	return b
}

func (b *RsBalancer) Start(sr <-chan SearchRequest) {
	log.Print("DEBUG: [RsBalancer] Start()")
	ticker := time.NewTicker(5 * time.Second)
	for {
		select {
		case req, ok := <-sr:
			if !ok {
				log.Print("INFO: all requests received!")
				return
			}
			b.dispatch(req)
		case res := <-b.done:
			b.completed(res)
		case <-ticker.C:
			b.print()
		}
	}
}

func (b *RsBalancer) print() {
	sum := 0
	dist := make(map[int]int)
	for _, rs := range b.pool {
		sum += rs.pending
		dist[rs.pending] += 1
	}
	log.Printf("DEBUG: [RsBalancer] %d requests pending. %v", sum, dist)
	log.Printf("DEBUG: [RsBalancer] %d in done queue.", len(b.done))
}

func (b *RsBalancer) dispatch(req SearchRequest) {
	rs := heap.Pop(&b.pool).(*RibSearcher)
	rs.requests <- req
	rs.pending++
	heap.Push(&b.pool, rs)
}

func (b *RsBalancer) completed(rs *RibSearcher) {
	rs.pending--
	heap.Remove(&b.pool, rs.i)
	heap.Push(&b.pool, rs)
}

func main() {
	runtime.GOMAXPROCS(8)

	if len(os.Args[1:]) != SearchIpNum {
		log.Printf("FATAL: %d IPv4 addresses required as arguments: %v", SearchIpNum, os.Args[1:])
		os.Exit(1)
	}
	var reqIp []net.IP
	for _, v := range os.Args[1:] {
		ip := net.ParseIP(v)
		if ip == nil || ip.To4() == nil {
			log.Printf("FATAL: invalid IPv4 address in arguments: %v", v)
			os.Exit(1)
		}
		reqIp = append(reqIp, ip)
	}

	req := make(chan SearchRequest)
	go func() {
		for i := 0; ; i++ {
			req <- SearchRequest{i, reqIp, make(chan []*table.Destination)}
			time.Sleep(IntervalMicrosec * time.Microsecond)
		}
		close(req)
	}()
	NewRsBalancer().Start(req)
}

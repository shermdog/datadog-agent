// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package network

import (
	"bytes"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/cihub/seelog"

	"github.com/DataDog/datadog-agent/pkg/network/dns"
	"github.com/DataDog/datadog-agent/pkg/network/protocols"
	"github.com/DataDog/datadog-agent/pkg/network/protocols/http"
	"github.com/DataDog/datadog-agent/pkg/network/protocols/kafka"
	nettelemetry "github.com/DataDog/datadog-agent/pkg/network/telemetry"
	"github.com/DataDog/datadog-agent/pkg/process/util"
	"github.com/DataDog/datadog-agent/pkg/telemetry"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

var (
	_ State = &networkState{}
)

// Telemetry
var stateTelemetry = struct {
	closedConnDropped     *nettelemetry.StatCounterWrapper
	connDropped           *nettelemetry.StatCounterWrapper
	statsUnderflows       *nettelemetry.StatCounterWrapper
	statsCookieCollisions *nettelemetry.StatCounterWrapper
	timeSyncCollisions    *nettelemetry.StatCounterWrapper
	dnsStatsDropped       *nettelemetry.StatCounterWrapper
	httpStatsDropped      *nettelemetry.StatCounterWrapper
	http2StatsDropped     *nettelemetry.StatCounterWrapper
	kafkaStatsDropped     *nettelemetry.StatCounterWrapper
	dnsPidCollisions      *nettelemetry.StatCounterWrapper
	udpDirectionFixes     telemetry.Counter
}{
	nettelemetry.NewStatCounterWrapper(stateModuleName, "closed_conn_dropped", []string{"ip_proto"}, "Counter measuring the number of dropped closed connections"),
	nettelemetry.NewStatCounterWrapper(stateModuleName, "conn_dropped", []string{}, "Counter measuring the number of closed connections"),
	nettelemetry.NewStatCounterWrapper(stateModuleName, "stats_underflows", []string{}, "Counter measuring the number of stats underflows"),
	nettelemetry.NewStatCounterWrapper(stateModuleName, "stats_cookie_collisions", []string{}, "Counter measuring the number of stats cookie collisions"),
	nettelemetry.NewStatCounterWrapper(stateModuleName, "time_sync_collisions", []string{}, "Counter measuring the number of time sync collisions"),
	nettelemetry.NewStatCounterWrapper(stateModuleName, "dns_stats_dropped", []string{}, "Counter measuring the number of DNS stats dropped"),
	nettelemetry.NewStatCounterWrapper(stateModuleName, "http_stats_dropped", []string{}, "Counter measuring the number of http stats dropped"),
	nettelemetry.NewStatCounterWrapper(stateModuleName, "http2_stats_dropped", []string{}, "Counter measuring the number of http2 stats dropped"),
	nettelemetry.NewStatCounterWrapper(stateModuleName, "kafka_stats_dropped", []string{}, "Counter measuring the number of kafka stats dropped"),
	nettelemetry.NewStatCounterWrapper(stateModuleName, "dns_pid_collisions", []string{}, "Counter measuring the number of DNS PID collisions"),
	telemetry.NewCounter(stateModuleName, "udp_direction_fixes", []string{}, "Counter measuring the number of udp direction fixes"),
}

const (
	// DEBUGCLIENT is the ClientID for debugging
	DEBUGCLIENT = "-1"

	// DNSResponseCodeNoError is the value that indicates that the DNS reply contains no errors.
	// We could have used layers.DNSResponseCodeNoErr here. But importing the gopacket library only for this
	// constant is not worth the increased memory cost.
	DNSResponseCodeNoError = 0

	// ConnectionByteKeyMaxLen represents the maximum size in bytes of a connection byte key
	ConnectionByteKeyMaxLen = 41

	stateModuleName = "network_tracer__state"
)

// State takes care of handling the logic for:
// - closed connections
// - sent and received bytes per connection
type State interface {
	// GetDelta returns a Delta object for the given client when provided the latest set of active connections
	GetDelta(
		clientID string,
		latestTime uint64,
		active []ConnectionStats,
		dns dns.StatsByKeyByNameByType,
		usmStats map[protocols.ProtocolType]interface{},
	) Delta

	// GetTelemetryDelta returns the telemetry delta since last time the given client requested telemetry data.
	GetTelemetryDelta(
		id string,
		telemetry map[ConnTelemetryType]int64,
	) map[ConnTelemetryType]int64

	// RegisterClient starts tracking stateful data for the given client
	// If the client is already registered, it does nothing.
	RegisterClient(clientID string)

	// RemoveClient stops tracking stateful data for a given client
	RemoveClient(clientID string)

	// RemoveExpiredClients removes expired clients from the state
	RemoveExpiredClients(now time.Time)

	// RemoveConnections removes the given keys from the state
	RemoveConnections(conns []*ConnectionStats)

	// StoreClosedConnections stores a batch of closed connections
	StoreClosedConnections(connections []ConnectionStats)

	// GetStats returns a map of statistics about the current network state
	GetStats() map[string]interface{}

	// DumpState returns a map with the current network state for a client ID
	DumpState(clientID string) map[string]interface{}
}

// Delta represents a delta of network data compared to the last call to State.
type Delta struct {
	BufferedData
	HTTP     map[http.Key]*http.RequestStats
	HTTP2    map[http.Key]*http.RequestStats
	Kafka    map[kafka.Key]*kafka.RequestStat
	DNSStats dns.StatsByKeyByNameByType
}

type lastStateTelemetry struct {
	closedConnDropped     int64
	connDropped           int64
	statsUnderflows       int64
	statsCookieCollisions int64
	timeSyncCollisions    int64
	dnsStatsDropped       int64
	httpStatsDropped      int64
	http2StatsDropped     int64
	kafkaStatsDropped     int64
	dnsPidCollisions      int64
}

const minClosedCapacity = 1024

type client struct {
	lastFetch time.Time

	closedConnectionsKeys map[StatCookie]int

	closedConnections []ConnectionStats
	stats             map[StatCookie]StatCounters
	// maps by dns key the domain (string) to stats structure
	dnsStats        dns.StatsByKeyByNameByType
	httpStatsDelta  map[http.Key]*http.RequestStats
	http2StatsDelta map[http.Key]*http.RequestStats
	kafkaStatsDelta map[kafka.Key]*kafka.RequestStat
	lastTelemetries map[ConnTelemetryType]int64
}

func (c *client) Reset(active map[StatCookie]*ConnectionStats) {
	half := cap(c.closedConnections) / 2
	if closedLen := len(c.closedConnections); closedLen > minClosedCapacity && closedLen < half {
		c.closedConnections = make([]ConnectionStats, half)
	}

	c.closedConnections = c.closedConnections[:0]
	c.closedConnectionsKeys = make(map[StatCookie]int)
	c.dnsStats = make(dns.StatsByKeyByNameByType)
	c.httpStatsDelta = make(map[http.Key]*http.RequestStats)
	c.http2StatsDelta = make(map[http.Key]*http.RequestStats)
	c.kafkaStatsDelta = make(map[kafka.Key]*kafka.RequestStat)

	// XXX: we should change the way we clean this map once
	// https://github.com/golang/go/issues/20135 is solved
	newStats := make(map[StatCookie]StatCounters, len(c.stats))
	for cookie, st := range c.stats {
		// Only keep active connections stats
		if _, isActive := active[cookie]; isActive {
			newStats[cookie] = st
		}
	}
	c.stats = newStats
}

type networkState struct {
	sync.Mutex

	// clients is a map of the connection id string to the client structure
	clients       map[string]*client
	lastTelemetry lastStateTelemetry // Old telemetry state; used for logging

	latestTimeEpoch uint64

	// Network state configuration
	clientExpiry   time.Duration
	maxClosedConns uint32
	maxClientStats int
	maxDNSStats    int
	maxHTTPStats   int
	maxKafkaStats  int

	mergeStatsBuffers [2][]byte
}

// NewState creates a new network state
func NewState(clientExpiry time.Duration, maxClosedConns uint32, maxClientStats int, maxDNSStats int, maxHTTPStats int, maxKafkaStats int) State {
	return &networkState{
		clients:        map[string]*client{},
		clientExpiry:   clientExpiry,
		maxClosedConns: maxClosedConns,
		maxClientStats: maxClientStats,
		maxDNSStats:    maxDNSStats,
		maxHTTPStats:   maxHTTPStats,
		maxKafkaStats:  maxKafkaStats,
		mergeStatsBuffers: [2][]byte{
			make([]byte, ConnectionByteKeyMaxLen),
			make([]byte, ConnectionByteKeyMaxLen),
		},
	}
}

func (ns *networkState) getClients() []string {
	ns.Lock()
	defer ns.Unlock()
	clients := make([]string, 0, len(ns.clients))

	for id := range ns.clients {
		clients = append(clients, id)
	}

	return clients
}

// GetTelemetryDelta returns the telemetry delta for a given client.
// As for now, this only keeps track of monotonic telemetry, as the
// other ones are already relative to the last time they were fetched.
func (ns *networkState) GetTelemetryDelta(
	id string,
	telemetry map[ConnTelemetryType]int64,
) map[ConnTelemetryType]int64 {
	ns.Lock()
	defer ns.Unlock()

	if len(telemetry) > 0 {
		return ns.getTelemetryDelta(id, telemetry)
	}
	return nil
}

// GetDelta returns the connections for the given client
// If the client is not registered yet, we register it and return the connections we have in the global state
// Otherwise we return both the connections with last stats and the closed connections for this client
func (ns *networkState) GetDelta(
	id string,
	latestTime uint64,
	active []ConnectionStats,
	dnsStats dns.StatsByKeyByNameByType,
	usmStats map[protocols.ProtocolType]interface{},
) Delta {
	ns.Lock()
	defer ns.Unlock()

	// Update the latest known time
	ns.latestTimeEpoch = latestTime
	connsByKey := ns.getConnsByCookie(active)

	clientBuffer := clientPool.Get(id)
	client := ns.getClient(id)
	defer client.Reset(connsByKey)

	// Update all connections with relevant up-to-date stats for client
	ns.mergeConnections(id, connsByKey, clientBuffer)

	conns := clientBuffer.Connections()
	ns.determineConnectionIntraHost(conns)
	if len(dnsStats) > 0 {
		ns.storeDNSStats(dnsStats)
	}

	for protocolType, protocolStats := range usmStats {
		switch protocolType {
		case protocols.HTTP:
			stats := protocolStats.(map[http.Key]*http.RequestStats)
			ns.storeHTTPStats(stats)
		case protocols.Kafka:
			stats := protocolStats.(map[kafka.Key]*kafka.RequestStat)
			ns.storeKafkaStats(stats)
		case protocols.HTTP2:
			stats := protocolStats.(map[http.Key]*http.RequestStats)
			ns.storeHTTP2Stats(stats)
		}
	}

	return Delta{
		BufferedData: BufferedData{
			Conns:  conns,
			buffer: clientBuffer,
		},
		HTTP:     client.httpStatsDelta,
		HTTP2:    client.http2StatsDelta,
		DNSStats: client.dnsStats,
		Kafka:    client.kafkaStatsDelta,
	}
}

// saveTelemetry saves the non-monotonic telemetry data for each registered clients.
// It does so by accumulating values per telemetry point.
func (ns *networkState) saveTelemetry(telemetry map[ConnTelemetryType]int64) {
	for _, cl := range ns.clients {
		for _, telType := range ConnTelemetryTypes {
			if val, ok := telemetry[telType]; ok {
				cl.lastTelemetries[telType] += val
			}
		}
	}
}

func (ns *networkState) getTelemetryDelta(id string, telemetry map[ConnTelemetryType]int64) map[ConnTelemetryType]int64 {
	ns.logTelemetry()

	var res = make(map[ConnTelemetryType]int64)
	client := ns.getClient(id)
	ns.saveTelemetry(telemetry)

	for _, telType := range MonotonicConnTelemetryTypes {
		if val, ok := telemetry[telType]; ok {
			res[telType] = val
			if prev, ok := client.lastTelemetries[telType]; ok {
				res[telType] -= prev
			}
			client.lastTelemetries[telType] = val
		}
	}

	for _, telType := range ConnTelemetryTypes {
		if _, ok := client.lastTelemetries[telType]; ok {
			res[telType] = client.lastTelemetries[telType]
			client.lastTelemetries[telType] = 0
		}
	}

	return res
}

func (ns *networkState) logTelemetry() {
	closedConnDroppedDelta := stateTelemetry.closedConnDropped.Load() - ns.lastTelemetry.closedConnDropped
	connDroppedDelta := stateTelemetry.connDropped.Load() - ns.lastTelemetry.connDropped
	statsUnderflowsDelta := stateTelemetry.statsUnderflows.Load() - ns.lastTelemetry.statsUnderflows
	statsCookieCollisionsDelta := stateTelemetry.statsCookieCollisions.Load() - ns.lastTelemetry.statsCookieCollisions
	timeSyncCollisionsDelta := stateTelemetry.timeSyncCollisions.Load() - ns.lastTelemetry.timeSyncCollisions
	dnsStatsDroppedDelta := stateTelemetry.dnsStatsDropped.Load() - ns.lastTelemetry.dnsStatsDropped
	httpStatsDroppedDelta := stateTelemetry.httpStatsDropped.Load() - ns.lastTelemetry.httpStatsDropped
	http2StatsDroppedDelta := stateTelemetry.http2StatsDropped.Load() - ns.lastTelemetry.http2StatsDropped
	kafkaStatsDroppedDelta := stateTelemetry.kafkaStatsDropped.Load() - ns.lastTelemetry.kafkaStatsDropped
	dnsPidCollisionsDelta := stateTelemetry.dnsPidCollisions.Load() - ns.lastTelemetry.dnsPidCollisions

	// Flush log line if any metric is non-zero
	if connDroppedDelta > 0 || closedConnDroppedDelta > 0 || dnsStatsDroppedDelta > 0 ||
		httpStatsDroppedDelta > 0 || http2StatsDroppedDelta > 0 || kafkaStatsDroppedDelta > 0 {
		s := "State telemetry: "
		s += " [%d connections dropped due to stats]"
		s += " [%d closed connections dropped]"
		s += " [%d DNS stats dropped]"
		s += " [%d HTTP stats dropped]"
		s += " [%d HTTP2 stats dropped]"
		s += " [%d Kafka stats dropped]"
		log.Warnf(s,
			connDroppedDelta,
			closedConnDroppedDelta,
			dnsStatsDroppedDelta,
			httpStatsDroppedDelta,
			http2StatsDroppedDelta,
			kafkaStatsDroppedDelta,
		)
	}

	// debug metrics that aren't useful for customers to see
	if statsCookieCollisionsDelta > 0 || statsUnderflowsDelta > 0 ||
		timeSyncCollisionsDelta > 0 || dnsPidCollisionsDelta > 0 {
		s := "State telemetry debug: "
		s += " [%d stats cookie collisions]"
		s += " [%d stats underflows]"
		s += " [%d time sync collisions]"
		s += " [%d DNS pid collisions]"
		log.Debugf(s,
			statsCookieCollisionsDelta,
			statsUnderflowsDelta,
			timeSyncCollisionsDelta,
			dnsPidCollisionsDelta,
		)
	}

	ns.lastTelemetry.closedConnDropped = stateTelemetry.closedConnDropped.Load()
	ns.lastTelemetry.connDropped = stateTelemetry.connDropped.Load()
	ns.lastTelemetry.statsUnderflows = stateTelemetry.statsUnderflows.Load()
	ns.lastTelemetry.statsCookieCollisions = stateTelemetry.statsCookieCollisions.Load()
	ns.lastTelemetry.timeSyncCollisions = stateTelemetry.timeSyncCollisions.Load()
	ns.lastTelemetry.dnsStatsDropped = stateTelemetry.dnsStatsDropped.Load()
	ns.lastTelemetry.httpStatsDropped = stateTelemetry.httpStatsDropped.Load()
	ns.lastTelemetry.http2StatsDropped = stateTelemetry.http2StatsDropped.Load()
	ns.lastTelemetry.kafkaStatsDropped = stateTelemetry.kafkaStatsDropped.Load()
	ns.lastTelemetry.dnsPidCollisions = stateTelemetry.dnsPidCollisions.Load()
}

// RegisterClient registers a client before it first gets stream of data.
// This call is not strictly mandatory, although it is useful when users
// want to first register and then start getting data at regular intervals.
// If the client is already registered, this call simply does nothing.
// The purpose of this new method is to start registering closed connections
// for the given client once this call has been made.
func (ns *networkState) RegisterClient(id string) {
	ns.Lock()
	defer ns.Unlock()

	_ = ns.getClient(id)
}

// getConnsByCookie returns a mapping of cookie -> connection for easier access + manipulation
func (ns *networkState) getConnsByCookie(conns []ConnectionStats) map[StatCookie]*ConnectionStats {
	connsByKey := make(map[StatCookie]*ConnectionStats, len(conns))
	for i := range conns {
		var c *ConnectionStats
		if c = connsByKey[conns[i].Cookie]; c == nil {
			connsByKey[conns[i].Cookie] = &conns[i]
			continue
		}

		if log.ShouldLog(seelog.TraceLvl) {
			log.Tracef("duplicate connection in collection: cookie: %d, c1: %+v, c2: %+v", c.Cookie, *c, conns[i])
		}

		if ns.mergeConnectionStats(c, &conns[i]) {
			// cookie collision
			stateTelemetry.statsCookieCollisions.Inc()
			// pick the latest one
			if conns[i].LastUpdateEpoch > c.LastUpdateEpoch {
				connsByKey[conns[i].Cookie] = &conns[i]
			}
		}
	}

	return connsByKey
}

func (ns *networkState) StoreClosedConnections(closed []ConnectionStats) {
	ns.Lock()
	defer ns.Unlock()

	ns.storeClosedConnections(closed)
}

// storeClosedConnection stores the given connection for every client
func (ns *networkState) storeClosedConnections(conns []ConnectionStats) {
	for _, client := range ns.clients {
		for _, c := range conns {
			if i, ok := client.closedConnectionsKeys[c.Cookie]; ok {
				if ns.mergeConnectionStats(&client.closedConnections[i], &c) {
					stateTelemetry.statsCookieCollisions.Inc()
					// pick the latest one
					if c.LastUpdateEpoch > client.closedConnections[i].LastUpdateEpoch {
						client.closedConnections[i] = c
					}
				}
				continue
			}

			if uint32(len(client.closedConnections)) >= ns.maxClosedConns {
				stateTelemetry.closedConnDropped.Inc(c.Type.String())
				continue
			}

			client.closedConnections = append(client.closedConnections, c)
			client.closedConnectionsKeys[c.Cookie] = len(client.closedConnections) - 1
		}
	}
}

func getDeepDNSStatsCount(stats dns.StatsByKeyByNameByType) int {
	var count int
	for _, bykey := range stats {
		for _, bydomain := range bykey {
			count += len(bydomain)
		}
	}
	return count
}

// storeDNSStats stores latest DNS stats for all clients
func (ns *networkState) storeDNSStats(stats dns.StatsByKeyByNameByType) {
	// Fast-path for common case (one client registered)
	if len(ns.clients) == 1 {
		for _, c := range ns.clients {
			if len(c.dnsStats) == 0 {
				c.dnsStats = stats
			}
			return
		}
	}

	for _, client := range ns.clients {
		dnsStatsThisClient := getDeepDNSStatsCount(client.dnsStats)
		for key, statsByDomain := range stats {
			for domain, statsByQtype := range statsByDomain {
				for qtype, dnsStats := range statsByQtype {

					if _, ok := client.dnsStats[key]; !ok {
						if dnsStatsThisClient >= ns.maxDNSStats {
							stateTelemetry.dnsStatsDropped.Inc()
							continue
						}
						client.dnsStats[key] = make(map[dns.Hostname]map[dns.QueryType]dns.Stats)
					}
					if _, ok := client.dnsStats[key][domain]; !ok {
						if dnsStatsThisClient >= ns.maxDNSStats {
							stateTelemetry.dnsStatsDropped.Inc()
							continue
						}
						client.dnsStats[key][domain] = make(map[dns.QueryType]dns.Stats)
					}

					// If we've seen DNS stats for this key already, let's combine the two
					if prev, ok := client.dnsStats[key][domain][qtype]; ok {
						prev.Timeouts += dnsStats.Timeouts
						prev.SuccessLatencySum += dnsStats.SuccessLatencySum
						prev.FailureLatencySum += dnsStats.FailureLatencySum
						for rcode, count := range dnsStats.CountByRcode {
							prev.CountByRcode[rcode] += count
						}
						client.dnsStats[key][domain][qtype] = prev
					} else {
						if dnsStatsThisClient >= ns.maxDNSStats {
							stateTelemetry.dnsStatsDropped.Inc()
							continue
						}
						client.dnsStats[key][domain][qtype] = dnsStats
						dnsStatsThisClient++
					}
				}
			}
		}
	}
}

// storeHTTPStats stores the latest HTTP stats for all clients
func (ns *networkState) storeHTTPStats(allStats map[http.Key]*http.RequestStats) {
	if len(ns.clients) == 1 {
		for _, client := range ns.clients {
			if len(client.httpStatsDelta) == 0 {
				// optimization for the common case:
				// if there is only one client and no previous state, no memory allocation is needed
				client.httpStatsDelta = allStats
				return
			}
		}
	}

	for key, stats := range allStats {
		for _, client := range ns.clients {
			prevStats, ok := client.httpStatsDelta[key]
			if !ok && len(client.httpStatsDelta) >= ns.maxHTTPStats {
				stateTelemetry.httpStatsDropped.Inc()
				continue
			}

			if prevStats != nil {
				prevStats.CombineWith(stats)
				client.httpStatsDelta[key] = prevStats
			} else {
				client.httpStatsDelta[key] = stats
			}
		}
	}
}

func (ns *networkState) storeHTTP2Stats(allStats map[http.Key]*http.RequestStats) {
	if len(ns.clients) == 1 {
		for _, client := range ns.clients {
			if len(client.http2StatsDelta) == 0 {
				// optimization for the common case:
				// if there is only one client and no previous state, no memory allocation is needed
				client.http2StatsDelta = allStats
				return
			}
		}
	}

	for key, stats := range allStats {
		for _, client := range ns.clients {
			prevStats, ok := client.http2StatsDelta[key]
			// Currently, we are using maxHTTPStats for HTTP2.
			if !ok && len(client.http2StatsDelta) >= ns.maxHTTPStats {
				stateTelemetry.http2StatsDropped.Inc()
				continue
			}

			if prevStats != nil {
				prevStats.CombineWith(stats)
				client.http2StatsDelta[key] = prevStats
			} else {
				client.http2StatsDelta[key] = stats
			}
		}
	}
}

// storeKafkaStats stores the latest Kafka stats for all clients
func (ns *networkState) storeKafkaStats(allStats map[kafka.Key]*kafka.RequestStat) {
	if len(ns.clients) == 1 {
		for _, client := range ns.clients {
			if len(client.kafkaStatsDelta) == 0 {
				// optimization for the common case:
				// if there is only one client and no previous state, no memory allocation is needed
				client.kafkaStatsDelta = allStats
				return
			}
		}
	}

	for key, stats := range allStats {
		for _, client := range ns.clients {
			prevStats, ok := client.kafkaStatsDelta[key]
			if !ok && len(client.kafkaStatsDelta) >= ns.maxKafkaStats {
				stateTelemetry.kafkaStatsDropped.Inc()
				continue
			}

			if prevStats != nil {
				prevStats.CombineWith(stats)
				client.kafkaStatsDelta[key] = prevStats
			} else {
				client.kafkaStatsDelta[key] = stats
			}
		}
	}
}

func (ns *networkState) getClient(clientID string) *client {
	if c, ok := ns.clients[clientID]; ok {
		return c
	}

	c := &client{
		lastFetch:             time.Now(),
		stats:                 make(map[StatCookie]StatCounters),
		closedConnections:     make([]ConnectionStats, 0, minClosedCapacity),
		closedConnectionsKeys: make(map[StatCookie]int),
		dnsStats:              dns.StatsByKeyByNameByType{},
		httpStatsDelta:        map[http.Key]*http.RequestStats{},
		http2StatsDelta:       map[http.Key]*http.RequestStats{},
		kafkaStatsDelta:       map[kafka.Key]*kafka.RequestStat{},
		lastTelemetries:       make(map[ConnTelemetryType]int64),
	}
	ns.clients[clientID] = c
	return c
}

// mergeConnections return the connections and takes care of updating their last stat counters
func (ns *networkState) mergeConnections(id string, active map[StatCookie]*ConnectionStats, buffer *clientBuffer) {
	now := time.Now()

	client := ns.clients[id]
	client.lastFetch = now

	// connections aggregated by tuple
	closed := client.closedConnections
	aggrConns := newConnectionAggregator(len(closed))
	for i := range closed {
		closedConn := &closed[i]
		cookie := closedConn.Cookie
		if activeConn := active[cookie]; activeConn != nil {
			if ns.mergeConnectionStats(closedConn, activeConn) {
				stateTelemetry.statsCookieCollisions.Inc()
				// remove any previous stats since we
				// can't distinguish between the two sets of stats
				delete(client.stats, cookie)
				if activeConn.LastUpdateEpoch > closedConn.LastUpdateEpoch {
					// keep active connection
					continue
				}

				// keep closed connection
			}
			// not an active connection
			delete(active, cookie)
		}

		ns.updateConnWithStats(client, cookie, closedConn)

		if closedConn.Last.IsZero() {
			continue
		}

		if !aggrConns.Aggregate(closedConn) {
			*buffer.Next() = *closedConn
		}
	}

	aggrConns.WriteTo(buffer)

	aggrConns = newConnectionAggregator(len(active))
	// Active connections
	for cookie, c := range active {
		ns.createStatsForCookie(client, cookie)
		ns.updateConnWithStats(client, cookie, c)

		if c.Last.IsZero() {
			continue
		}

		if !aggrConns.Aggregate(c) {
			*buffer.Next() = *c
		}
	}

	aggrConns.WriteTo(buffer)
}

func (ns *networkState) updateConnWithStats(client *client, cookie StatCookie, c *ConnectionStats) {
	c.Last = StatCounters{}
	if sts, ok := client.stats[cookie]; ok {
		var last StatCounters
		var underflow bool
		if last, underflow = c.Monotonic.Sub(sts); underflow {
			stateTelemetry.statsUnderflows.Inc()
			log.DebugFunc(func() string {
				return fmt.Sprintf("Stats underflow for cookie:%d, stats counters:%+v, connection counters:%+v", c.Cookie, sts, c.Monotonic)
			})

			c.Monotonic = c.Monotonic.Max(sts)
			last, _ = c.Monotonic.Sub(sts)
		}

		c.Last = c.Last.Add(last)
		client.stats[cookie] = c.Monotonic
	} else {
		c.Last = c.Last.Add(c.Monotonic)
	}
}

// createStatsForCookie will create a new stats object for a key if it doesn't already exist.
func (ns *networkState) createStatsForCookie(client *client, cookie StatCookie) {
	if _, ok := client.stats[cookie]; !ok {
		if len(client.stats) >= ns.maxClientStats {
			stateTelemetry.connDropped.Inc()
			return
		}

		client.stats[cookie] = StatCounters{}
	}
}

func (ns *networkState) RemoveClient(clientID string) {
	ns.Lock()
	defer ns.Unlock()
	delete(ns.clients, clientID)
	clientPool.RemoveExpiredClient(clientID)
}

func (ns *networkState) RemoveExpiredClients(now time.Time) {
	ns.Lock()
	defer ns.Unlock()

	for id, c := range ns.clients {
		if c.lastFetch.Add(ns.clientExpiry).Before(now) {
			log.Debugf("expiring client: %s, had %d stats and %d closed connections", id, len(c.stats), len(c.closedConnections))
			delete(ns.clients, id)
			clientPool.RemoveExpiredClient(id)
		}
	}
}

func (ns *networkState) RemoveConnections(conns []*ConnectionStats) {
	ns.Lock()
	defer ns.Unlock()

	for _, cl := range ns.clients {
		for _, c := range conns {
			delete(cl.stats, c.Cookie)
		}
	}
}

// GetStats returns a map of statistics about the current network state
func (ns *networkState) GetStats() map[string]interface{} {
	ns.Lock()
	defer ns.Unlock()

	clientInfo := map[string]interface{}{}
	for id, c := range ns.clients {
		clientInfo[id] = map[string]int{
			"stats":              len(c.stats),
			"closed_connections": len(c.closedConnections),
			"last_fetch":         int(c.lastFetch.Unix()),
		}
	}

	return map[string]interface{}{
		"clients": clientInfo,
		"telemetry": map[string]int64{
			"closed_conn_dropped": stateTelemetry.closedConnDropped.Load(),
			"conn_dropped":        stateTelemetry.connDropped.Load(),
			"dns_stats_dropped":   stateTelemetry.dnsStatsDropped.Load(),
		},
		"current_time":       time.Now().Unix(),
		"latest_bpf_time_ns": ns.latestTimeEpoch,
	}
}

// DumpState returns the entirety of the network state in memory at the moment for a particular clientID, for debugging
func (ns *networkState) DumpState(clientID string) map[string]interface{} {
	ns.Lock()
	defer ns.Unlock()

	data := map[string]interface{}{}
	if client, ok := ns.clients[clientID]; ok {
		for cookie, s := range client.stats {
			data[strconv.Itoa(int(cookie))] = map[string]uint64{
				"total_sent":            s.SentBytes,
				"total_recv":            s.RecvBytes,
				"total_retransmits":     uint64(s.Retransmits),
				"total_tcp_established": uint64(s.TCPEstablished),
				"total_tcp_closed":      uint64(s.TCPClosed),
			}
		}
	}
	return data
}

func isDNAT(c *ConnectionStats) bool {
	return c.Direction == OUTGOING &&
		c.IPTranslation != nil &&
		(c.IPTranslation.ReplSrcIP.Compare(c.Dest.Addr) != 0 ||
			c.IPTranslation.ReplSrcPort != c.DPort)
}

func (ns *networkState) determineConnectionIntraHost(connections []ConnectionStats) {
	type connKey struct {
		Address util.Address
		Port    uint16
		Type    ConnectionType
	}

	newConnKey := func(connStat *ConnectionStats, useRAddrAsKey bool) connKey {
		key := connKey{Type: connStat.Type}
		if useRAddrAsKey {
			if connStat.IPTranslation == nil {
				key.Address = connStat.Dest
				key.Port = connStat.DPort
			} else {
				key.Address = connStat.IPTranslation.ReplSrcIP
				key.Port = connStat.IPTranslation.ReplSrcPort
			}
		} else {
			key.Address = connStat.Source
			key.Port = connStat.SPort
		}
		return key
	}

	type dnatKey struct {
		src, dst     util.Address
		sport, dport uint16
		_type        ConnectionType
	}

	dnats := make(map[dnatKey]struct{}, len(connections)/2)
	lAddrs := make(map[connKey]struct{}, len(connections))
	for i := range connections {
		conn := &connections[i]
		k := newConnKey(conn, false)
		lAddrs[k] = struct{}{}

		if isDNAT(conn) {
			dnats[dnatKey{
				src:   conn.Source,
				sport: conn.SPort,
				dst:   conn.IPTranslation.ReplSrcIP,
				dport: conn.IPTranslation.ReplSrcPort,
				_type: conn.Type,
			}] = struct{}{}
		}
	}

	// do not use range value here since it will create a copy of the ConnectionStats object
	for i := range connections {
		conn := &connections[i]
		if conn.Source == conn.Dest ||
			(conn.Source.IsLoopback() && conn.Dest.IsLoopback()) ||
			(conn.IPTranslation != nil && conn.IPTranslation.ReplSrcIP.IsLoopback()) {
			conn.IntraHost = true
		} else {
			keyWithRAddr := newConnKey(conn, true)
			_, conn.IntraHost = lAddrs[keyWithRAddr]
		}

		fixConnectionDirection(conn)

		if conn.IntraHost &&
			conn.Direction == INCOMING &&
			conn.IPTranslation != nil {
			// Remove ip translation from incoming local connections
			// this is necessary for local connections because of
			// the way we store conntrack entries in the conntrack
			// cache in the system-probe. For local connections
			// that are DNAT'ed, system-probe will tack on the
			// translation on the incoming source side as well,
			// even though there is no SNAT on the incoming side.
			// This is because we store both the origin and reply
			// (and map them to each other) in the conntrack cache
			// in system-probe.

			// check if this connection is also dnat'ed before
			// zero'ing out the ip translation
			// note: src/dst address/port are reversed since
			// we are looking for the outgoing side of this
			// incoming connection
			if _, ok := dnats[dnatKey{
				src:   conn.Dest,
				sport: conn.DPort,
				dst:   conn.Source,
				dport: conn.SPort,
				_type: conn.Type,
			}]; ok {
				conn.IPTranslation = nil
			}
		}
	}
}

// fixConnectionDirection fixes connection direction
// for UDP incoming connections.
//
// Some UDP connections can be assigned an incoming
// direction incorrectly since we cannot reliably
// distinguish between a server and client for UDP
// in eBPF. Both clients and servers can call
// the system call bind() for source ports, but
// UDP servers don't call listen() or accept()
// like TCP.
//
// This function fixes only a very specific case:
// incoming UDP connections, when the source
// port is ephemeral but the destination port is not.
// This is the only case where we can be sure the
// connection has the incorrect direction of
// incoming. For remote connections, only
// destination ports < 1024 are considered
// non-ephemeral.
func fixConnectionDirection(c *ConnectionStats) {
	// fix only incoming UDP connections
	if c.Direction != INCOMING || c.Type != UDP {
		return
	}

	sourceEphemeral := IsPortInEphemeralRange(c.Family, c.Type, c.SPort) == EphemeralTrue
	var destNotEphemeral bool
	if c.IntraHost {
		destNotEphemeral = IsPortInEphemeralRange(c.Family, c.Type, c.DPort) != EphemeralTrue
	} else {
		// use a much more restrictive range
		// for non-ephemeral ports if the
		// connection is not local
		destNotEphemeral = c.DPort < 1024
	}
	if sourceEphemeral && destNotEphemeral {
		c.Direction = OUTGOING
		stateTelemetry.udpDirectionFixes.Inc()
	}
}

type connectionAggregator struct {
	conns map[string]*struct {
		*ConnectionStats
		rttSum, rttVarSum uint64
		count             uint32
	}
	buf []byte
}

func newConnectionAggregator(size int) *connectionAggregator {
	return &connectionAggregator{
		conns: make(map[string]*struct {
			*ConnectionStats
			rttSum    uint64
			rttVarSum uint64
			count     uint32
		}, size),
		buf: make([]byte, ConnectionByteKeyMaxLen),
	}
}

// Aggregate aggregates a connection. The connection is only
// aggregated if:
// - it is not in the collection
// - it is in the collection and:
//   - the ip translation is nil OR
//   - the other connection's ip translation is nil OR
//   - the other connection's ip translation is not nil AND the nat info is the same
func (a *connectionAggregator) Aggregate(c *ConnectionStats) bool {
	key := string(c.ByteKey(a.buf))
	aggrConn, ok := a.conns[key]
	if !ok {
		a.conns[key] = &struct {
			*ConnectionStats
			rttSum    uint64
			rttVarSum uint64
			count     uint32
		}{
			ConnectionStats: c,
			rttSum:          uint64(c.RTT),
			rttVarSum:       uint64(c.RTTVar),
			count:           1,
		}

		return true
	}

	if !(aggrConn.IPTranslation == nil ||
		c.IPTranslation == nil ||
		*c.IPTranslation == *aggrConn.IPTranslation) {
		return false
	}

	aggrConn.Monotonic = aggrConn.Monotonic.Add(c.Monotonic)
	aggrConn.Last = aggrConn.Last.Add(c.Last)
	aggrConn.rttSum += uint64(c.RTT)
	aggrConn.rttVarSum += uint64(c.RTTVar)
	aggrConn.count++
	if aggrConn.LastUpdateEpoch < c.LastUpdateEpoch {
		aggrConn.LastUpdateEpoch = c.LastUpdateEpoch
	}
	if aggrConn.IPTranslation == nil {
		aggrConn.IPTranslation = c.IPTranslation
	}

	return true
}

// WriteTo writes the aggregated connections to a clientBuffer,
// computing an average for RTT and RTTVar for each
// connection
func (a connectionAggregator) WriteTo(buffer *clientBuffer) {
	for _, c := range a.conns {
		c.RTT = uint32(c.rttSum / uint64(c.count))
		c.RTTVar = uint32(c.rttVarSum / uint64(c.count))
		*buffer.Next() = *c.ConnectionStats
	}
}

func (ns *networkState) mergeConnectionStats(a, b *ConnectionStats) (collision bool) {
	if a.Cookie != b.Cookie {
		return false
	}

	if bytes.Compare(a.ByteKey(ns.mergeStatsBuffers[0]), b.ByteKey(ns.mergeStatsBuffers[1])) != 0 {
		log.Debugf("cookie collision for connections %+v and %+v", a, b)
		// cookie collision
		return true
	}

	a.Monotonic = a.Monotonic.Max(b.Monotonic)

	if b.LastUpdateEpoch > a.LastUpdateEpoch {
		a.LastUpdateEpoch = b.LastUpdateEpoch
	}

	if a.IPTranslation == nil {
		a.IPTranslation = b.IPTranslation
	}

	a.ProtocolStack.MergeWith(b.ProtocolStack)

	return false
}

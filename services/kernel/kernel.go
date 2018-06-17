package kernel

/*
#include "common.h"
#cgo CFLAGS: -D_GNU_SOURCE
#cgo LDFLAGS: -lnetfilter_queue -lnfnetlink -lnetfilter_conntrack -lnetfilter_log
*/
import "C"

import (
	"encoding/binary"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/untangle/packetd/services/reports"
	"github.com/untangle/packetd/services/support"
	"net"
	"sync"
	"time"
	"unsafe"
)

// To give C child functions access we export go_child_startup and shutdown functions.
var childsync sync.WaitGroup
var appname = "kernel"
var shutdownChannel = make(chan bool)

// Startup starts C services
func Startup() {
	// Load the conndict module
	support.SystemCommand("modprobe", []string{"nf_conntrack_dict"})

	C.common_startup()
}

// StartCallbacks donates threads for all the C services
// after this all callbacks will be called using these threads
func StartCallbacks() {
	// Donate threads to kernel hooks
	go C.netfilter_thread()
	go C.conntrack_thread()
	go C.netlogger_thread()

	// start the conntrack 60-second update task
	go periodicTask()
}

// StopCallbacks stops all services and callbacks
func StopCallbacks() {
	// Remove all kernel hooks
	go C.netfilter_shutdown()
	go C.conntrack_shutdown()
	go C.netlogger_shutdown()

	// Send shutdown signal to periodicTask and wait for it to return
	shutdownChannel <- true
	select {
	case <-shutdownChannel:
	case <-time.After(10 * time.Second):
		support.LogMessage(support.LogErr, appname, "Failed to properly shutdown periodicTask\n")
	}

	// wait on above shutdowns
	childsync.Wait()
}

// Shutdown all C services
func Shutdown() {
	C.common_shutdown()
}

// GetShutdownFlag returns the c shutdown flag
func GetShutdownFlag() int {
	return int(C.get_shutdown_flag())
}

//export go_netfilter_callback
func go_netfilter_callback(mark C.uint, data *C.uchar, size C.int, ctid C.uint) uint32 {
	var mess support.TrafficMessage

	// ***** this version creates a Go copy of the buffer = SLOWER
	// buffer := C.GoBytes(unsafe.Pointer(data),size)

	// ***** this version creates a Go pointer to the buffer = FASTER
	buffer := (*[0xFFFF]byte)(unsafe.Pointer(data))[:int(size):int(size)]

	// convert the C length and ctid to Go values
	connid := uint(ctid)

	// get the existing mark on the packet
	var pmark uint32 = uint32(C.int(mark))

	// make a gopacket from the raw packet data
	mess.Packet = gopacket.NewPacket(buffer, layers.LayerTypeIPv4, gopacket.DecodeOptions{Lazy: true, NoCopy: true})
	mess.Length = int(size)

	// get the IPv4 layer
	ipLayer := mess.Packet.Layer(layers.LayerTypeIPv4)
	if ipLayer == nil {
		return (pmark)
	}
	mess.IPlayer = ipLayer.(*layers.IPv4)

	mess.Tuple.Protocol = uint8(mess.IPlayer.Protocol)
	mess.Tuple.ClientAddr = mess.IPlayer.SrcIP
	mess.Tuple.ServerAddr = mess.IPlayer.DstIP

	// get the TCP layer
	tcpLayer := mess.Packet.Layer(layers.LayerTypeTCP)
	if tcpLayer != nil {
		mess.TCPlayer = tcpLayer.(*layers.TCP)
		mess.Tuple.ClientPort = uint16(mess.TCPlayer.SrcPort)
		mess.Tuple.ServerPort = uint16(mess.TCPlayer.DstPort)
	}

	// get the UDP layer
	udpLayer := mess.Packet.Layer(layers.LayerTypeUDP)
	if udpLayer != nil {
		mess.UDPlayer = udpLayer.(*layers.UDP)
		mess.Tuple.ClientPort = uint16(mess.UDPlayer.SrcPort)
		mess.Tuple.ServerPort = uint16(mess.UDPlayer.DstPort)
	}

	// right now we only care about TCP and UDP
	// FIXME do we really?
	if (tcpLayer == nil) && (udpLayer == nil) {
		return (pmark)
	}

	// get the Application layer
	appLayer := mess.Packet.ApplicationLayer()
	if appLayer != nil {
		mess.Payload = appLayer.Payload()
	}

	var session support.SessionEntry
	var ok bool
	var newSession = false

	// If we already have a session entry update the existing, otherwise create a new entry for the table.
	if session, ok = support.FindSessionEntry(uint32(ctid)); ok {
		support.LogMessage(support.LogDebug, appname, "SESSION Found %d in table\n", ctid)
		session.SessionActivity = time.Now()
		session.UpdateCount++
	} else {
		support.LogMessage(support.LogDebug, appname, "SESSION Adding %d to table\n", ctid)
		newSession = true
		session.SessionID = support.NextSessionID()
		session.SessionCreation = time.Now()
		session.SessionActivity = time.Now()
		session.ClientSideTuple = mess.Tuple
		session.UpdateCount = 1
		support.AttachNetfilterSubscriptions(&session)
		support.InsertSessionEntry(uint32(ctid), session)
	}

	mess.Session = session

	pipe := make(chan support.SubscriptionResult)

	// We loop and increment the priority until all subscribtions have been called
	subtotal := len(session.NetfilterSubs)
	subcount := 0
	priority := 0

	for subcount != subtotal {
		// Counts the total number of calls made for each priority so we know
		// how many SubscriptionResult's to read from the result channel
		hitcount := 0

		// Call all of the subscribed handlers for the current priority
		for key, val := range session.NetfilterSubs {
			if val.Priority != priority {
				continue
			}
			support.LogMessage(support.LogDebug, appname, "Calling netfilter APP:%s PRIORITY:%d\n", key, priority)
			go val.NetfilterFunc(pipe, mess, connid)
			hitcount++
			subcount++
		}

		// Add the mark bits returned from each handler and remove the session
		// subscription for any that set the SessionRelease flag
		for i := 0; i < hitcount; i++ {
			select {
			case result := <-pipe:
				pmark |= result.PacketMark
				if result.SessionRelease {
					support.LogMessage(support.LogDebug, appname, "Removing %s session netfilter subscription for %d\n", result.Owner, uint32(ctid))
					delete(session.NetfilterSubs, result.Owner)
				}
			}
		}

		// Increment the priority and keep looping until we've called all subscribers
		priority++
	}

	if newSession {
		// FIXME time_stamp
		// FIXME local_addr
		// FIXME remote_addr
		// FIXME client_intf
		// FIXME server_intf
		columns := map[string]interface{}{
			"session_id":  session.SessionID,
			"ip_protocol": session.ClientSideTuple.Protocol,
			"client_addr": session.ClientSideTuple.ClientAddr,
			"server_addr": session.ClientSideTuple.ServerAddr,
			"client_port": session.ClientSideTuple.ClientPort,
			"server_port": session.ClientSideTuple.ServerPort,
		}
		reports.LogEvent(reports.CreateEvent("session_new", "sessions", 1, columns, nil))
	}

	// return the updated mark to be set on the packet
	return (pmark)
}

//export go_conntrack_callback
func go_conntrack_callback(info *C.struct_conntrack_info) {
	var clientSideTuple support.Tuple
	var ctid uint32

	clientSideTuple.Protocol = uint8(info.orig_proto)
	// FIXME IPv6
	clientSideTuple.ClientAddr = make(net.IP, 4)
	binary.LittleEndian.PutUint32(clientSideTuple.ClientAddr, uint32(info.orig_saddr))
	clientSideTuple.ClientPort = uint16(info.orig_sport)
	// FIXME IPv6
	clientSideTuple.ServerAddr = make(net.IP, 4)
	binary.LittleEndian.PutUint32(clientSideTuple.ServerAddr, uint32(info.orig_daddr))
	clientSideTuple.ServerPort = uint16(info.orig_dport)

	ctid = uint32(info.conn_id)
	session, sessionFound := support.FindSessionEntry(uint32(ctid))

	// FIXME, This can be removed if we are sure this never happens.
	// This is temporary and is used to look for conntrack id's being re-used
	// unexpectedly. On the first packet, the netfilter handler seems to get
	// called first, before the conntrack handler, so we use the ctid in that
	// handler to create the session entry. It's possible we'll get the
	// conntrack NEW message before the session gets added by the other
	// handler, so we don't care if the session is not found, but if we find
	// the session and the update count is greater than one, it likely means a
	// conntrack ID has been reused, and we need to re-think some things.
	if info.msg_type == 'N' && sessionFound && session.UpdateCount != 1 {
		// FIXME, I suspect on a multi-core machine, it is possible to process 2 packets before conntrack NEW
		// in this case, this would fire, but not be a reused conntrack ID - dmorris
		support.LogMessage(support.LogWarn, appname, "Unexperted update Count %d for Session %d\n", session.UpdateCount, ctid)
		panic("CONNTRACK ID RE-USE DETECTED")
	}

	// If we already have a conntrackEntry update the existing, otherwise create a new conntrackEntry for the table.
	conntrackEntry, conntrackEntryFound := support.FindConntrackEntry(ctid)
	if conntrackEntryFound {
		support.LogMessage(support.LogDebug, appname, "CONNTRACK Found %d in table\n", ctid)
		conntrackEntry.UpdateCount++
	} else {
		conntrackEntry.ConntrackID = ctid
		conntrackEntry.SessionID = support.NextSessionID()
		conntrackEntry.SessionCreation = time.Now()
		conntrackEntry.ClientSideTuple = clientSideTuple
		conntrackEntry.UpdateCount = 1
		support.InsertConntrackEntry(ctid, conntrackEntry)
	}

	conntrackEntry.SessionActivity = time.Now()

	// handle NEW events
	if info.msg_type == 'N' {
		if sessionFound {
			var serverSideTuple support.Tuple
			serverSideTuple.Protocol = uint8(info.orig_proto)
			// FIXME IPv6
			serverSideTuple.ClientAddr = make(net.IP, 4)
			binary.LittleEndian.PutUint32(serverSideTuple.ClientAddr, uint32(info.repl_daddr))
			serverSideTuple.ClientPort = uint16(info.repl_dport)
			// FIXME IPv6
			serverSideTuple.ServerAddr = make(net.IP, 4)
			binary.LittleEndian.PutUint32(serverSideTuple.ServerAddr, uint32(info.repl_saddr))
			serverSideTuple.ServerPort = uint16(info.repl_sport)

			// Set the server side tuple, this is the first time we've seen the post-NAT data
			session.ServerSideTuple = serverSideTuple

			columns := map[string]interface{}{
				"session_id": session.SessionID,
			}
			modifiedColumns := map[string]interface{}{
				"client_addr_new": session.ServerSideTuple.ClientAddr,
				"server_addr_new": session.ServerSideTuple.ServerAddr,
				"client_port_new": session.ServerSideTuple.ClientPort,
				"server_port_new": session.ServerSideTuple.ServerPort,
			}
			reports.LogEvent(reports.CreateEvent("session_nat", "sessions", 2, columns, modifiedColumns))
		} else {
			// We should not receive a new conntrack event for something that is not in the session table
			// However it happens on local outbound sessions, we should handle these diffently
			// FIXME log session_new event
		}
	}

	// handle DELETE events
	if info.msg_type == 'D' {
		conntrackEntry.PurgeFlag = true
		support.LogMessage(support.LogDebug, appname, "SESSION Removing %d from table\n", ctid)
		support.RemoveSessionEntry(ctid)
	} else {
		conntrackEntry.PurgeFlag = false
	}

	// handle UPDATE events
	if info.msg_type == 'U' {
		oldC2sBytes := conntrackEntry.C2Sbytes
		oldS2cBytes := conntrackEntry.S2Cbytes
		oldTotalBytes := conntrackEntry.TotalBytes
		newC2sBytes := uint64(info.orig_bytes)
		newS2cBytes := uint64(info.repl_bytes)
		newTotalBytes := (newC2sBytes + newS2cBytes)
		diffC2sBytes := (newC2sBytes - oldC2sBytes)
		diffS2cBytes := (newS2cBytes - oldS2cBytes)
		diffTotalBytes := (newTotalBytes - oldTotalBytes)

		// In some cases, specifically UDP, a new session takes the place of an old session with the same tuple.
		// In this case the counts go down because its actually a new session. If the total bytes is low, this
		// is probably the case so treat it as a new conntrackEntry.
		if (diffC2sBytes < 0) || (diffS2cBytes < 0) {
			newC2sBytes = uint64(info.orig_bytes)
			diffC2sBytes = newC2sBytes
			newS2cBytes = uint64(info.repl_bytes)
			diffS2cBytes = newS2cBytes
			newTotalBytes = (newC2sBytes + newS2cBytes)
			diffTotalBytes = newTotalBytes
			return
		}

		c2sRate := float32(diffC2sBytes / 60)
		s2cRate := float32(diffS2cBytes / 60)
		totalRate := float32(diffTotalBytes / 60)

		conntrackEntry.C2Sbytes = newC2sBytes
		conntrackEntry.S2Cbytes = newS2cBytes
		conntrackEntry.TotalBytes = newTotalBytes
		conntrackEntry.C2Srate = c2sRate
		conntrackEntry.S2Crate = s2cRate
		conntrackEntry.TotalRate = totalRate

		// FIXME log session_minutes event
	}

	// We loop and increment the priority until all subscribtions have been called
	sublist := support.GetConntrackSubscriptions()
	subtotal := len(sublist)
	subcount := 0
	priority := 0

	for subcount != subtotal {
		// Call all of the subscribed handlers for the current priority
		for key, val := range sublist {
			if val.Priority != priority {
				continue
			}
			support.LogMessage(support.LogDebug, appname, "Calling conntrack APP:%s PRIORITY:%d\n", key, priority)
			go val.ConntrackFunc(int(info.msg_type), &conntrackEntry)
			subcount++
		}

		// Increment the priority and keep looping until we've called all subscribers
		priority++
	}
}

//export go_netlogger_callback
func go_netlogger_callback(info *C.struct_netlogger_info) {
	var logger support.NetloggerMessage

	logger.Version = uint8(info.version)
	logger.Protocol = uint8(info.protocol)
	logger.IcmpType = uint16(info.icmp_type)
	logger.SrcIntf = uint8(info.src_intf)
	logger.DstIntf = uint8(info.dst_intf)
	logger.SrcAddr = C.GoString(&info.src_addr[0])
	logger.DstAddr = C.GoString(&info.dst_addr[0])
	logger.SrcPort = uint16(info.src_port)
	logger.DstPort = uint16(info.dst_port)
	logger.Mark = uint32(info.mark)
	logger.Prefix = C.GoString(&info.prefix[0])

	// We loop and increment the priority until all subscribtions have been called
	sublist := support.GetNetloggerSubscriptions()
	subtotal := len(sublist)
	subcount := 0
	priority := 0

	for subcount != subtotal {
		// Call all of the subscribed handlers for the current priority
		for key, val := range sublist {
			if val.Priority != priority {
				continue
			}
			support.LogMessage(support.LogDebug, appname, "Calling netlogger APP:%s PRIORITY:%d\n", key, priority)
			go val.NetloggerFunc(&logger)
			subcount++
		}

		// Increment the priority and keep looping until we've called all subscribers
		priority++
	}
}

//export go_child_startup
func go_child_startup() {
	childsync.Add(1)
}

//export go_child_shutdown
func go_child_shutdown() {
	childsync.Done()
}

//export go_child_message
func go_child_message(level C.int, source *C.char, message *C.char) {
	lsrc := C.GoString(source)
	lmsg := C.GoString(message)
	support.LogMessage(int(level), lsrc, lmsg)
}

//conntrack periodic task
func periodicTask() {
	var counter int

	// FIXME first sleep should be calibrated so it wakes on first second on minute for conntrack_dump
	for {
		select {
		case <-shutdownChannel:
			shutdownChannel <- true
			return
		case <-time.After(60 * time.Second):
			counter++
			support.LogMessage(support.LogDebug, appname, "Calling periodic conntrack dump %d\n", counter)
			C.conntrack_dump()
		}
	}
}

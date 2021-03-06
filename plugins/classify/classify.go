// Package classify classifies sessions as certain applications
// each packet gets sent to a classd daemon (the categorization engine)
// the classd daemon returns the classification information and classify
// attaches the information to the session.
package classify

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/untangle/packetd/services/dict"
	"github.com/untangle/packetd/services/dispatch"
	"github.com/untangle/packetd/services/logger"
	"github.com/untangle/packetd/services/reports"
)

// applicationInfo stores the details for each know application
type applicationInfo struct {
	guid         string
	index        int
	name         string
	description  string
	category     string
	productivity int
	risk         int
	flags        uint64
	reference    string
	plugin       string
}

const pluginName = "classify"
const guidInfoFile = "/usr/share/untangle-classd/protolist.csv"

var applicationTable map[string]applicationInfo

const navlStateTerminated = 0 // Indicates the connection has been terminated
const navlStateInspecting = 1 // Indicates the connection is under inspection
const navlStateMonitoring = 2 // Indicates the connection is under monitoring
const navlStateClassified = 3 // Indicates the connection is fully classified

const maxPacketCount = 64      // The maximum number of packets to inspect before releasing
const maxTrafficSize = 0x10000 // The maximum number of bytes to inspect before releasing

type daemonSignal int

const (
	daemonNoop daemonSignal = iota
	daemonStartup
	daemonShutdown
	daemonFinished
	socketConnect
	systemStartup
	systemShutdown
)

var processChannel = make(chan daemonSignal, 1)
var socketChannel = make(chan daemonSignal, 1)
var shutdownChannel = make(chan bool)
var classdHostPort = "127.0.0.1:8123"

// PluginStartup is called to allow plugin specific initialization
func PluginStartup() {
	var err error
	var info os.FileInfo

	logger.Info("PluginStartup(%s) has been called\n", pluginName)

	//  make sure the classd binary is available
	info, err = os.Stat(daemonBinary)
	if err != nil {
		logger.Notice("Unable to check status of classify daemon %s (%v)\n", daemonBinary, err)
		return
	}

	//  make sure the classd binary is executable
	if (info.Mode() & 0111) == 0 {
		logger.Notice("Invalid file mode for classify daemon %s (%v)\n", daemonBinary, info.Mode())
		return
	}

	// load the application details
	loadApplicationTable()

	// start the daemon manager to handle running the daemon process
	go daemonProcessManager()

	// start the socket manager to handle the daemon socket connection
	go daemonSocketManager()

	// insert our nfqueue subscription
	dispatch.InsertNfqueueSubscription(pluginName, dispatch.ClassifyPriority, PluginNfqueueHandler)
}

// PluginShutdown is called when the daemon is shutting down
func PluginShutdown() {
	logger.Info("PluginShutdown(%s) has been called\n", pluginName)

	// signal the socket manager that the system is shutting down
	signalSocketManager(systemShutdown)

	select {
	case <-shutdownChannel:
		logger.Info("Successful shutdown of daemonSocketManager\n")
	case <-time.After(10 * time.Second):
		logger.Warn("Failed to properly shutdown daemonSocketManager\n")
	}

	// signal the process manager that the system is shutting down
	signalProcessManager(systemShutdown)

	select {
	case <-shutdownChannel:
		logger.Info("Successful shutdown of daemonProcessManager\n")
	case <-time.After(10 * time.Second):
		logger.Warn("Failed to properly shutdown daemonProcessManager\n")
	}

}

// PluginNfqueueHandler is called for raw nfqueue packets. We pass the
// packet directly to the Sandvine NAVL library for classification, and
// push the results to the conntrack dictionary.
func PluginNfqueueHandler(mess dispatch.NfqueueMessage, ctid uint32, newSession bool) dispatch.NfqueueResult {
	var reply string

	// make sure we have a valid session
	if mess.Session == nil {
		logger.Err("Ignoring event with invalid Session\n")
		return dispatch.NfqueueResult{SessionRelease: true}
	}

	// make sure we have a valid session id
	if mess.Session.GetSessionID() == 0 {
		logger.Err("Ignoring event with invalid SessionID\n")
		return dispatch.NfqueueResult{SessionRelease: true}
	}

	// make sure we have a valid IPv4 or IPv6 layer
	if mess.IP4Layer == nil && mess.IP6Layer == nil {
		logger.Err("Invalid packet: %v\n", mess.Session.GetClientSideTuple())
		return dispatch.NfqueueResult{SessionRelease: true}
	}

	// send the data to classd and read reply
	reply = classifyTraffic(&mess)

	// an empty reply means we can't talk to the daemon so just release the session
	if len(reply) == 0 {
		return dispatch.NfqueueResult{SessionRelease: true}
	}

	// process the reply and get the classification state
	state, confidence := processReply(reply, mess, ctid)

	// if the daemon says the session is fully classified or terminated, or after we have seen maximum packets or data, release the session
	if state == navlStateClassified || state == navlStateTerminated || mess.Session.GetPacketCount() > maxPacketCount || mess.Session.GetByteCount() > maxTrafficSize {

		if logger.IsLogEnabled(logger.LogLevelDebug) {
			logger.Debug("RELEASING SESSION:%d STATE:%d CONFIDENCE:%d PACKETS:%d BYTES:%d\n", ctid, state, confidence, mess.Session.GetPacketCount(), mess.Session.GetByteCount())
		}

		return dispatch.NfqueueResult{SessionRelease: true}
	}

	return dispatch.NfqueueResult{SessionRelease: false}
}

// classifyTraffic sends the packet to the daemon manager for classification and returns the result
func classifyTraffic(mess *dispatch.NfqueueMessage) string {
	var command string
	var proto string
	var reply string

	if mess.IP4Layer != nil {
		proto = "IP4"
	} else if mess.IP6Layer != nil {
		proto = "IP6"
	} else {
		logger.Err("Unsupported protocol for %d\n", mess.Session.GetConntrackID())
		return ""
	}

	// send the packet to the daemon for classification
	command = fmt.Sprintf("PACKET|%d|%s|%d\r\n", mess.Session.GetSessionID(), proto, len(mess.Packet.Data()))
	reply = daemonClassifyPacket(command, mess.Packet.Data())
	return reply
}

// processReply processes a reply from the classd daemon
func processReply(reply string, mess dispatch.NfqueueMessage, ctid uint32) (int, uint64) {
	var appid string
	var name string
	var protochain string
	var detail string
	var confidence uint64
	var category string
	var state int
	var attachments map[string]interface{}

	// parse update classd information from reply
	appid, name, protochain, detail, confidence, category, state = parseReply(reply)

	// WARNING - DO NOT USE Session GetAttachment or SetAttachment in this function
	// Because we make decisions based on existing attachments and update multiple
	// attachments, we lock the attachments and access them directly for efficiency.
	// Other calls that lock the attachment mutex will hang forever if called from here.
	attachments = mess.Session.LockAttachments()
	defer mess.Session.UnlockAttachments()

	// We look at the confidence and ignore any reply where the value is less
	// than the confidence currently attached to the session. Because of the
	// unpredictable nature of gorouting scheduling, we sometimes get confidence = 0
	// if NAVL didn't give us any classification. This can happen if packets are
	// processed out of order and NAVL gets data for a session that has already
	// encountered a FIN packet. In this case it generates a no connection error
	// and classd gives us the generic /IP defaults. We also don't want to apply
	// a lower confidence reply on top of a higher confidence reply which can
	// happen if the lower confidence reply is received and parsed after the
	// higher confidence reply has already been handled.

	checkdata := attachments["application_confidence"]
	if checkdata != nil {
		checkval := checkdata.(uint64)
		if confidence < checkval {
			logger.Debug("Ignoring update with confidence %d < %d STATE:%d\n", confidence, checkval, state)
			return state, confidence
		}
	}
	checkprotochain := attachments["application_protochain"]
	if checkprotochain != nil {
		current := checkprotochain.(string)
		if strings.Count(protochain, "/") < strings.Count(current, "/") {
			logger.Debug("Ignoring update with protochain %s < %s STATE:%d\n", protochain, current, state)
			return state, confidence
		}
	}

	var changed []string
	if updateClassifyDetail(attachments, ctid, "application_id", appid) {
		changed = append(changed, "application_id")
	}
	if updateClassifyDetail(attachments, ctid, "application_name", name) {
		changed = append(changed, "application_name")
	}
	if updateClassifyDetail(attachments, ctid, "application_protochain", protochain) {
		changed = append(changed, "application_protochain")
	}
	if updateClassifyDetail(attachments, ctid, "application_detail", detail) {
		changed = append(changed, "application_detail")
	}
	if updateClassifyDetail(attachments, ctid, "application_confidence", confidence) {
		changed = append(changed, "application_confidence")
	}
	if updateClassifyDetail(attachments, ctid, "application_category", category) {
		changed = append(changed, "application_category")
	}

	// if something changed, log a new event
	if len(changed) > 0 {
		logEvent(mess.Session, attachments, changed)
	}

	return state, confidence
}

// parseReply parses a reply from classd and returns
// (appid, name, protochain, detail, confidence, category, state)
func parseReply(replyString string) (string, string, string, string, uint64, string, int) {
	var err error
	var appid string
	var name string
	var protochain string
	var detail string
	var confidence uint64
	var category string
	var state int

	rawinfo := strings.Split(replyString, "\r\n")

	for i := 0; i < len(rawinfo); i++ {
		if len(rawinfo[i]) < 3 {
			continue
		}
		rawpair := strings.SplitAfter(rawinfo[i], ": ")
		if len(rawpair) != 2 {
			continue
		}

		switch rawpair[0] {
		case "APPLICATION: ":
			appid = rawpair[1]
		case "PROTOCHAIN: ":
			protochain = rawpair[1]
		case "DETAIL: ":
			detail = rawpair[1]
		case "CONFIDENCE: ":
			confidence, err = strconv.ParseUint(rawpair[1], 10, 64)
			if err != nil {
				confidence = 0
			}
		case "STATE: ":
			state, err = strconv.Atoi(rawpair[1])
			if err != nil {
				state = 0
			}
		}
	}

	// lookup the category in the application table
	appinfo, finder := applicationTable[appid]
	if finder == true {
		name = appinfo.name
		category = appinfo.category
	}

	return appid, name, protochain, detail, confidence, category, state

}

// logEvent logs a session_classify event that updates the application_* columns
// provide the session and the changed column names
func logEvent(session *dispatch.Session, attachments map[string]interface{}, changed []string) {
	if len(changed) == 0 {
		return
	}
	columns := map[string]interface{}{
		"session_id": session.GetSessionID(),
	}
	modifiedColumns := make(map[string]interface{})
	for _, v := range changed {
		modifiedColumns[v] = attachments[v]
	}

	reports.LogEvent(reports.CreateEvent("session_classify", "sessions", 2, columns, modifiedColumns))
}

// loadApplicationTable loads the details for each application
func loadApplicationTable() {
	var file *os.File
	var linecount int
	var infocount int
	var list []string
	var err error

	applicationTable = make(map[string]applicationInfo)

	// open the guid info file provided by Sandvine
	file, err = os.Open(guidInfoFile)

	// if there was an error log and return
	if err != nil {
		logger.Warn("Unable to load application details: %s\n", guidInfoFile)
		return
	}

	// create a new CSV reader
	reader := csv.NewReader(bufio.NewReader(file))
	for {
		list, err = reader.Read()

		if err == io.EOF {
			// on end of file just break out of the read loop
			break
		} else if err != nil {
			// for anything else log the error and break
			logger.Err("Unable to parse application details: %v\n", err)
			break
		}

		// count the number of lines read so we can compare with
		// the number successfully parsed when we finish loading
		linecount++

		// skip the first line that holds the file format description
		if linecount == 1 {
			continue
		}

		// if we did not parse exactly 10 fields skip the line
		if len(list) != 10 {
			logger.Warn("Invalid line length: %d\n", len(list))
			continue
		}

		var info applicationInfo

		info.guid = list[0]
		info.index, err = strconv.Atoi(list[1])
		if err != nil {
			logger.Warn("Invalid index: %s\n", list[1])
		}
		info.name = list[2]
		info.description = list[3]
		info.category = list[4]
		info.productivity, err = strconv.Atoi(list[5])
		if err != nil {
			logger.Warn("Invalid productivity: %s\n", list[5])
		}
		info.risk, err = strconv.Atoi(list[6])
		if err != nil {
			logger.Warn("Invalid risk: %s\n", list[6])
		}
		info.flags, err = strconv.ParseUint(list[7], 10, 64)
		if err != nil {
			logger.Warn("Invalid flags: %s %s\n", list[7], err)
		}
		info.reference = list[8]
		info.plugin = list[9]

		applicationTable[list[0]] = info
		infocount++
	}

	file.Close()
	logger.Info("Loaded classification details for %d applications\n", infocount)

	// if there were any bad lines in the file log a warning
	if infocount != linecount-1 {
		logger.Warn("Detected garbage in the application info file: %s\n", guidInfoFile)
	}
}

// updateClassifyDetail updates a key/value pair in the session attachments
// if the value has changed for the provided key, it will also update the nf_dict session table
// returns true if value changed, false otherwise
func updateClassifyDetail(attachments map[string]interface{}, ctid uint32, pairname string, pairdata interface{}) bool {

	// we don't wan't to put empty strings in the attachments or the dictionary
	switch v := pairdata.(type) {
	case string:
		if len(v) > 0 {
			break
		}
		logger.Trace("Empty classification detail for %s\n", pairname)
		return false
	}

	// if the session doesn't have this attachment yet we add it and write to the dictionary
	checkdata := attachments[pairname]
	if checkdata == nil {
		attachments[pairname] = pairdata
		dict.AddSessionEntry(ctid, pairname, pairdata)
		logger.Debug("Setting classification detail %s = %v ctid:%d\n", pairname, pairdata, ctid)
		return true
	}

	// if the session has the attachment and it has not changed just return
	if checkdata == pairdata {
		if logger.IsTraceEnabled() {
			logger.Trace("Ignoring classification detail %s = %v ctid:%d\n", pairname, pairdata, ctid)
		}
		return false
	}

	// at this point the session has the attachment but the data has changed so we update the session and the dictionary
	attachments[pairname] = pairdata
	dict.AddSessionEntry(ctid, pairname, pairdata)
	logger.Debug("Updating classification detail %s from %v to %v ctid:%d\n", pairname, checkdata, pairdata, ctid)
	return true
}

// SetHostPort sets the address for the classdDaemon. Default is "127.0.0.1:8123"
func SetHostPort(value string) {
	classdHostPort = value
}

// signalProcessManager sends a signal to the daemon manager goroutine
func signalProcessManager(signal daemonSignal) {
	select {
	case processChannel <- signal:
	default:
	}
}

func signalSocketManager(signal daemonSignal) {
	select {
	case socketChannel <- signal:
	default:
	}
}

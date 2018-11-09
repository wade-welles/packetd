package dispatch

import (
	"github.com/untangle/packetd/services/dict"
	"github.com/untangle/packetd/services/logger"
	"sync"
	"time"
)

// SessionEntry stores details related to a session
type SessionEntry struct {
	SessionID          uint64
	ConntrackID        uint32
	PacketCount        uint64
	ByteCount          uint64
	CreationTime       time.Time
	LastActivityTime   time.Time
	ClientSideTuple    Tuple
	ServerSideTuple    Tuple
	ConntrackConfirmed bool
	EventCount         uint64
	subscriptions      map[string]SubscriptionHolder
	subLocker          sync.Mutex
	attachments        map[string]interface{}
	attachmentLock     sync.Mutex
}

var sessionTable map[string]*SessionEntry
var sessionMutex sync.Mutex
var sessionIndex uint64

// PutAttachment is used to safely add an attachment to a session object
func (entry *SessionEntry) PutAttachment(name string, value interface{}) {
	entry.attachmentLock.Lock()
	entry.attachments[name] = value
	entry.attachmentLock.Unlock()
}

// GetAttachment is used to safely get an attachment from a session object
func (entry *SessionEntry) GetAttachment(name string) interface{} {
	entry.attachmentLock.Lock()
	value := entry.attachments[name]
	entry.attachmentLock.Unlock()
	return value
}

// DeleteAttachment is used to safely delete an attachment from a session object
func (entry *SessionEntry) DeleteAttachment(name string) bool {
	entry.attachmentLock.Lock()
	value := entry.attachments[name]
	delete(entry.attachments, name)
	entry.attachmentLock.Unlock()

	if value == nil {
		return false
	}

	return true
}

// nextSessionID returns the next sequential session ID value
func nextSessionID() uint64 {
	var value uint64
	sessionMutex.Lock()
	value = sessionIndex
	sessionIndex++

	if sessionIndex == 0 {
		sessionIndex++
	}

	sessionMutex.Unlock()
	return (value)
}

// findSessionEntry searches for an entry in the session table
func findSessionEntry(finder string) (*SessionEntry, bool) {
	sessionMutex.Lock()
	entry, status := sessionTable[finder]
	logger.Trace("Lookup session index %s -> %v\n", finder, status)
	sessionMutex.Unlock()
	return entry, status
}

// insertSessionEntry adds an entry to the session table
func insertSessionEntry(finder string, entry *SessionEntry) {
	logger.Trace("Insert session index %s -> %v\n", finder, entry.ClientSideTuple)
	sessionMutex.Lock()
	defer sessionMutex.Unlock()
	if sessionTable[finder] != nil {
		delete(sessionTable, finder)
	}
	sessionTable[finder] = entry
	dict.AddSessionEntry(entry.ConntrackID, "session_id", entry.SessionID)
}

// removeSessionEntry removes an entry from the session table
func removeSessionEntry(finder string) {
	logger.Trace("Remove session index %s\n", finder)
	sessionMutex.Lock()
	defer sessionMutex.Unlock()
	entry, status := sessionTable[finder]
	if status {
		dict.DeleteSession(entry.ConntrackID)
	}
	delete(sessionTable, finder)
}

// cleanSessionTable cleans the session table by removing stale entries
func cleanSessionTable() {
	nowtime := time.Now()

	for key, val := range sessionTable {
		if (nowtime.Unix() - val.LastActivityTime.Unix()) < 600 {
			continue
		}
		removeSessionEntry(key)
		// This happens in some corner cases
		// such as a session that is blocked we will have a session in the session table
		// but it will never reach the conntrack confirmed state, and thus never
		// get a conntrack new or destroy event
		// as such this will exist in the table until the conntrack ID gets re-used
		// or this happens. Since this is condition is expected, just log as debug
		logger.Debug("Removing stale session entry %s %v\n", key, val.ClientSideTuple)
	}
}

// printSessionTable prints the session table
func printSessionTable() {
	sessionMutex.Lock()
	defer sessionMutex.Unlock()
	for k, v := range sessionTable {
		logger.Debug("Session[%s] = %s\n", k, v.ClientSideTuple.String())
	}
}

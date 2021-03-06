/**
 * conntrack.c
 *
 * Handles receiving conntrack updates for the Untangle Packet Daemon
 *
 * Copyright (c) 2018 Untangle, Inc.
 * All Rights Reserved
 */

#include "common.h"

static struct nfct_handle	*nfcth;
static u_int64_t			tracker_error;
static u_int64_t			tracker_unknown;
static u_int64_t			tracker_garbage;
static char                 *logsrc = "conntrack";

struct update_mark_args {
	uint32_t	ctid;
	uint32_t	mask;
	uint32_t	val;
};

#define BUFFER_SIZE 1024*1024*8

static int conntrack_callback(enum nf_conntrack_msg_type type,struct nf_conntrack *ct,void *data)
{
	struct conntrack_info	info;

	// if the shutdown flag is set return stop to interrupt nfct_catch
	if (get_shutdown_flag() != 0) return(NFCT_CB_STOP);

	switch (type) {
	case NFCT_T_NEW:
		info.msg_type = 'N';
		break;
	case NFCT_T_UPDATE:
		info.msg_type = 'U';
		break;
	case NFCT_T_DESTROY:
		info.msg_type = 'D';
		break;
	case NFCT_T_ERROR:
		tracker_error++;
		return(NFCT_CB_CONTINUE);
	default:
		tracker_unknown++;
		return(NFCT_CB_CONTINUE);
	}

	info.family = nfct_get_attr_u8(ct,ATTR_ORIG_L3PROTO);
	info.orig_proto = nfct_get_attr_u8(ct,ATTR_ORIG_L4PROTO);

	// get the conntrack ID
	info.conn_id = nfct_get_attr_u32(ct, ATTR_ID);

    //logmessage(LOG_TRACE, logsrc, "conntrack event [%u] %c\n", info.conn_id, info.msg_type);

	// get the orig and repl source and destination addresses
	if (info.family == AF_INET) {
		memcpy(&info.orig_saddr,nfct_get_attr(ct,ATTR_ORIG_IPV4_SRC),4);
		memcpy(&info.orig_daddr,nfct_get_attr(ct,ATTR_ORIG_IPV4_DST),4);
		memcpy(&info.repl_saddr,nfct_get_attr(ct,ATTR_REPL_IPV4_SRC),4);
		memcpy(&info.repl_daddr,nfct_get_attr(ct,ATTR_REPL_IPV4_DST),4);
	} else if (info.family == AF_INET6) {
		memcpy(&info.orig_saddr,nfct_get_attr(ct,ATTR_ORIG_IPV6_SRC),16);
		memcpy(&info.orig_daddr,nfct_get_attr(ct,ATTR_ORIG_IPV6_DST),16);
		memcpy(&info.repl_saddr,nfct_get_attr(ct,ATTR_REPL_IPV6_SRC),16);
		memcpy(&info.repl_daddr,nfct_get_attr(ct,ATTR_REPL_IPV6_DST),16);
	} else {
		tracker_garbage++;
		return(NFCT_CB_CONTINUE);
	}

	// get all of the source and destination ports
	info.orig_sport = be16toh(nfct_get_attr_u16(ct,ATTR_ORIG_PORT_SRC));
	info.orig_dport = be16toh(nfct_get_attr_u16(ct,ATTR_ORIG_PORT_DST));

	// get all of the source and destination ports
	info.repl_sport = be16toh(nfct_get_attr_u16(ct,ATTR_REPL_PORT_SRC));
	info.repl_dport = be16toh(nfct_get_attr_u16(ct,ATTR_REPL_PORT_DST));

	// get the byte counts
	info.orig_bytes = nfct_get_attr_u64(ct,ATTR_ORIG_COUNTER_BYTES);
	info.repl_bytes = nfct_get_attr_u64(ct,ATTR_REPL_COUNTER_BYTES);

    // get the packet counts
    info.orig_packets = nfct_get_attr_u64(ct,ATTR_ORIG_COUNTER_PACKETS);
    info.repl_packets = nfct_get_attr_u64(ct,ATTR_REPL_COUNTER_PACKETS);

    // get the timeout
    info.timeout = nfct_get_attr_u32(ct,ATTR_TIMEOUT);

    // get the timestamps
    info.timestamp_start = nfct_get_attr_u64(ct,ATTR_TIMESTAMP_START);
    info.timestamp_stop = nfct_get_attr_u64(ct,ATTR_TIMESTAMP_STOP);

    // get tcp state
    info.tcp_state = nfct_get_attr_u8(ct,ATTR_TCP_STATE);
    
    // get the mark
	info.conn_mark = nfct_get_attr_u32(ct,ATTR_MARK);

	if (get_warehouse_flag() == 'C') warehouse_capture('C',&info,sizeof(info),0,0,0,info.family);

    // FIXME - its not ok to just throw away events when the bypass flag is set
    // we will be missing important events like NEW/DELETE events such that
    // when we resume, the events will no longer make sense because we missed important events prior
	if (get_bypass_flag() != 0)
        return NFCT_CB_CONTINUE;

    go_conntrack_callback(&info,0);
	return NFCT_CB_CONTINUE;
}

int conntrack_startup(void)
{
	int		ret;

	// Open a netlink conntrack handle. The header file defines
	// NFCT_ALL_CT_GROUPS but we really only care about new and
	// destroy so we subscribe to just those ignoring update
	nfcth = nfct_open(CONNTRACK,NF_NETLINK_CONNTRACK_NEW | NF_NETLINK_CONNTRACK_DESTROY);

	if (nfcth == NULL) {
		logmessage(LOG_ERR,logsrc,"Error %d returned from nfct_open()\n",errno);
		set_shutdown_flag();
		return(1);
	}

    ret = nfnl_rcvbufsiz(nfct_nfnlh(nfcth), BUFFER_SIZE);
    logmessage(LOG_DEBUG,logsrc,"Buffer size set to %d\n", ret);

	// register the conntrack callback
	ret = nfct_callback_register(nfcth,NFCT_T_ALL,conntrack_callback,NULL);

	if (ret != 0) {
		logmessage(LOG_ERR,logsrc,"Error %d returned from nfct_callback_register()\n",errno);
		set_shutdown_flag();
		return(2);
	}

	return(0);
}

void conntrack_shutdown(void)
{
	if (nfcth == NULL) return;

    struct nfct_handle* ptr = nfcth;

	// clear our conntrack handle
    nfcth = NULL;

	// unregister the callback handler
	nfct_callback_unregister(ptr);

	// close the conntrack netlink handler
	nfct_close(ptr);
}

int conntrack_thread(void)
{
	struct timeval	tv;
	fd_set			tester;
	int				sock,ret;

	logmessage(LOG_INFO,logsrc,"The conntrack thread is starting\n");

	// call our conntrack startup function
	ret = conntrack_startup();

	if (ret != 0) {
		logmessage(LOG_ERR,logsrc,"Error %d returned from conntrack_startup()\n",ret);
		set_shutdown_flag();
		return(1);
	}

	go_child_startup();

	// set the file descriptor to non-blocking mode
	sock = nfct_fd(nfcth);
	fcntl(sock, F_SETFL, O_NONBLOCK);

	// detect and process events while the shutdown flag is clear
	while (get_shutdown_flag() == 0) {
        /* int res = nfct_catch(nfcth); */
        /* if (res < 0) { */
        /*     if (nfcth == NULL) */
        /*         return 0; */
        /* } */
        FD_ZERO(&tester);
		FD_SET(sock,&tester);
		tv.tv_sec = 1;
		tv.tv_usec = 0;
		ret = select(sock+1,&tester,NULL,NULL,&tv);
		if (ret < 1) continue;
		if (FD_ISSET(sock,&tester) == 0) continue;
		nfct_catch(nfcth);
	}

	// call our conntrack shutdown function
	conntrack_shutdown();

	logmessage(LOG_INFO,logsrc,"The conntrack thread has terminated\n");
	go_child_shutdown();
	return(0);
}

void conntrack_dump(void)
{
	u_int32_t	family;
	int			ret;

	if (nfcth == NULL) return;

	family = AF_UNSPEC;
	ret = nfct_send(nfcth,NFCT_Q_DUMP,&family);
	if (ret < 0) logmessage(LOG_WARNING,logsrc,"nfct_send() result:%d errno:%d\n",ret,errno);
}

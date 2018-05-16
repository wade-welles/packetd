/**
 * common.h
 *
 * Shared cgo variables and functions for the Untangle Packet Daemon
 *
 * Copyright (c) 2018 Untangle, Inc.
 * All Rights Reserved
 */

#define DATAFLAG
#include "common.h"
/*--------------------------------------------------------------------------*/
static struct timeval   g_runtime;
static int              g_shutdown;
static int              g_debug;
/*--------------------------------------------------------------------------*/
void common_startup(void)
{
    gettimeofday(&g_runtime,NULL);
    g_shutdown = 0;
    g_debug = 1;
}
/*--------------------------------------------------------------------------*/
void common_goodbye(void)
{
}
/*--------------------------------------------------------------------------*/
char* itolevel(int value,char *dest)
{
    if (value == LOG_EMERG)     return(strcpy(dest,"EMERGENCY"));
    if (value == LOG_ALERT)     return(strcpy(dest,"ALERT"));
    if (value == LOG_CRIT)      return(strcpy(dest,"CRITICAL"));
    if (value == LOG_ERR)       return(strcpy(dest,"ERROR"));
    if (value == LOG_WARNING)   return(strcpy(dest,"WARNING"));
    if (value == LOG_NOTICE)    return(strcpy(dest,"NOTICE"));
    if (value == LOG_INFO)      return(strcpy(dest,"INFO"));
    if (value == LOG_DEBUG)     return(strcpy(dest,"DEBUG"));

    sprintf(dest,"LOG_%d",value);
    return(dest);
}
/*--------------------------------------------------------------------------*/
void rawmessage(int priority,const char *message)
{
    if ((priority == LOG_DEBUG) && (g_debug == 0)) return;
    go_child_message((char *)message);
}
/*--------------------------------------------------------------------------*/
void logmessage(int priority,const char *format,...)
{
    va_list         args;
    char            message[1024];

    if ((priority == LOG_DEBUG) && (g_debug == 0)) return;

    va_start(args,format);
    vsnprintf(message,sizeof(message),format,args);
    va_end(args);

    rawmessage(priority,message);
}
/*--------------------------------------------------------------------------*/
void hexmessage(int priority,const void *buffer,int size)
{
    const unsigned char     *data;
    char                    *message;
    int                     loc;
    int                     x;

    if ((priority == LOG_DEBUG) && (g_debug == 0)) return;

    message = (char *)malloc((size * 3) + 4);
    data = (const unsigned char *)buffer;

    for (x = 0;x < size;x++) {
        loc = (x * 3);
        if (x == 0) sprintf(&message[loc],"%02X ",data[x]);
        else sprintf(&message[loc],"%02X ",data[x]);
    }

    loc = (size * 3);
    strcpy(&message[loc],"\n");
    rawmessage(priority,message);
    free(message);
}
/*--------------------------------------------------------------------------*/
int get_shutdown_flag(void)
{
    return(g_shutdown);
}
/*--------------------------------------------------------------------------*/
void set_shutdown_flag(int value)
{
    g_shutdown = value;
}
/*--------------------------------------------------------------------------*/

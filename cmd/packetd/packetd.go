package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/untangle/packetd/plugins/certfetch"
	"github.com/untangle/packetd/plugins/certsniff"
	"github.com/untangle/packetd/plugins/classify"
	"github.com/untangle/packetd/plugins/dns"
	"github.com/untangle/packetd/plugins/example"
	"github.com/untangle/packetd/plugins/geoip"
	"github.com/untangle/packetd/plugins/reporter"
	"github.com/untangle/packetd/plugins/revdns"
	"github.com/untangle/packetd/plugins/sni"
	"github.com/untangle/packetd/services/certcache"
	"github.com/untangle/packetd/services/dict"
	"github.com/untangle/packetd/services/dispatch"
	"github.com/untangle/packetd/services/kernel"
	"github.com/untangle/packetd/services/logger"
	"github.com/untangle/packetd/services/reports"
	"github.com/untangle/packetd/services/restd"
	"github.com/untangle/packetd/services/settings"
	"github.com/untangle/packetd/services/syscmd"
)

const rulesScript = "packetd_rules"

var memProfileTarget string
var localFlag bool

func main() {
	handleSignals()
	parseArguments()

	// Start services
	startServices()

	// Start the plugins
	logger.Info("Starting plugins...\n")
	startPlugins()

	// Start the callbacks AFTER all services and plugins are initialized
	logger.Info("Starting kernel callbacks...\n")
	kernel.StartCallbacks()

	// Insert netfilter rules
	logger.Info("Inserting netfilter rules...\n")
	insertRules()

	// If the local flag is set we start a goroutine to watch for console input.
	// This can be used to quickly/easily tell the application to terminate when
	// running under gdb to diagnose threads hanging at shutdown. This requires
	// something other than CTRL+C since that is intercepted by gdb, and debugging
	// those kind of issues can be timing sensitive, so it's often not helpful to
	// figure out the PID and send a signal from another console.
	if localFlag {
		logger.Notice("Running on console - Press enter to terminate\n")
		go func() {
			reader := bufio.NewReader(os.Stdin)
			reader.ReadString('\n')
			logger.Notice("Console input detected - Application shutting down\n")
			kernel.SetShutdownFlag()
		}()
	}

	if kernel.GetWarehouseFlag() == 'P' {
		dispatch.HandleWarehousePlayback()
	}

	if kernel.GetWarehouseFlag() == 'C' {
		kernel.StartWarehouseCapture()
	}

	// Wait until the shutdown flag is set
	for !kernel.GetShutdownFlag() {
		select {
		case <-kernel.GetShutdownChannel():
			break
		case <-time.After(1 * time.Hour):
			logger.Info(".\n")
			printStats()
		}
	}
	logger.Info("Shutdown initiated...\n")

	if kernel.GetWarehouseFlag() == 'C' {
		kernel.CloseWarehouseCapture()
	}

	// Remove netfilter rules
	logger.Info("Removing netfilter rules...\n")
	removeRules()

	// Stop kernel callbacks
	logger.Info("Removing kernel callbacks...\n")
	kernel.StopCallbacks()

	// Stop all plugins
	logger.Info("Stopping plugins...\n")
	stopPlugins()

	// Stop services
	logger.Info("Stopping services...\n")

	if len(memProfileTarget) > 0 {
		f, err := os.Create(memProfileTarget)
		if err == nil {
			runtime.GC()
			pprof.WriteHeapProfile(f)
			f.Close()
		}
	}

	stopServices()
}

func printVersion() {
	logger.Info("Untangle Packet Daemon Version %s\n", Version)
}

// parseArguments parses the command line arguments
func parseArguments() {
	classdAddressStringPtr := flag.String("classd", "127.0.0.1:8123", "host:port for classd daemon")
	disableConndictPtr := flag.Bool("disable-dict", false, "disable dict")
	versionPtr := flag.Bool("version", false, "version")
	localPtr := flag.Bool("local", false, "run on console")
	debugPtr := flag.Bool("debug", false, "enable debug")
	bypassPtr := flag.Bool("bypass", false, "ignore live traffic")
	playbackFilePtr := flag.String("playback", "", "playback traffic from specified file")
	captureFilePtr := flag.String("capture", "", "capture traffic to specified file")
	playSpeedPtr := flag.Int("playspeed", 100, "traffic playback speed percentage")
	cpuProfilePtr := flag.String("cpuprofile", "", "write cpu profile to file")
	memProfilePtr := flag.String("memprofile", "", "write memory profile to file")

	flag.Parse()

	classify.SetHostPort(*classdAddressStringPtr)

	if *disableConndictPtr {
		dict.Disable()
	}

	if *versionPtr {
		printVersion()
		os.Exit(0)
	}

	if *localPtr {
		localFlag = true
	}

	if *debugPtr {
		kernel.SetDebugFlag()
	}

	if *bypassPtr {
		kernel.SetBypassFlag(1)
	}

	if len(*playbackFilePtr) != 0 {
		kernel.SetWarehouseFile(*playbackFilePtr)
		kernel.SetWarehouseFlag('P')
	}

	if len(*captureFilePtr) != 0 {
		kernel.SetWarehouseFile(*captureFilePtr)
		kernel.SetWarehouseFlag('C')
	}

	if *playSpeedPtr != 1 {
		kernel.SetWarehouseSpeed(*playSpeedPtr)
	}

	if *cpuProfilePtr != "" {
		f, err := os.Create(*cpuProfilePtr)
		if err != nil {
			logger.Err("Could not create CPU profile: ", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			logger.Err("Could not start CPU profile: ", err)
		}
		defer pprof.StopCPUProfile()
	}

	if *memProfilePtr != "" {
		memProfileTarget = *memProfilePtr
	}
}

// startServices starts all the services
func startServices() {
	logger.Startup()
	logger.Info("Starting services...\n")
	printVersion()
	kernel.Startup()
	dispatch.Startup()
	syscmd.Startup()
	settings.Startup()
	reports.Startup()
	dict.Startup()
	restd.Startup()
	certcache.Startup()
}

// stopServices stops all the services
func stopServices() {
	c := make(chan bool)
	go func() {
		certcache.Shutdown()
		restd.Shutdown()
		dict.Shutdown()
		reports.Shutdown()
		settings.Shutdown()
		syscmd.Shutdown()
		dispatch.Shutdown()
		kernel.Shutdown()
		logger.Shutdown()
		c <- true
	}()

	select {
	case <-c:
	case <-time.After(10 * time.Second):
		// can't use logger as it may be stopped
		fmt.Printf("ERROR: Failed to properly shutdown services\n")
		time.Sleep(1 * time.Second)
	}
}

// startPlugins starts all the plugins (in parallel)
func startPlugins() {
	var wg sync.WaitGroup

	// Start Plugins
	startups := []func(){
		example.PluginStartup,
		classify.PluginStartup,
		geoip.PluginStartup,
		certfetch.PluginStartup,
		certsniff.PluginStartup,
		dns.PluginStartup,
		revdns.PluginStartup,
		sni.PluginStartup,
		reporter.PluginStartup}
	for _, f := range startups {
		wg.Add(1)
		go func(f func()) {
			f()
			wg.Done()
		}(f)
	}

	wg.Wait()
}

// stopPlugins stops all the plugins (in parallel)
func stopPlugins() {
	var wg sync.WaitGroup

	shutdowns := []func(){
		example.PluginShutdown,
		classify.PluginShutdown,
		geoip.PluginShutdown,
		certfetch.PluginShutdown,
		certsniff.PluginShutdown,
		dns.PluginShutdown,
		revdns.PluginShutdown,
		sni.PluginShutdown,
		reporter.PluginShutdown}
	for _, f := range shutdowns {
		wg.Add(1)
		go func(f func()) {
			f()
			wg.Done()
		}(f)
	}

	wg.Wait()
}

// Add signal handlers
func handleSignals() {
	// Add SIGINT & SIGTERM handler (exit)
	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-ch
		logger.Warn("Received signal [%v]. Setting shutdown flag\n", sig)
		kernel.SetShutdownFlag()
	}()

	// Add SIGQUIT handler (dump thread stack trace)
	quitch := make(chan os.Signal, 1)
	signal.Notify(quitch, syscall.SIGQUIT)
	go func() {
		for {
			sig := <-quitch
			buf := make([]byte, 1<<20)
			logger.Warn("Received signal [%v]. Printing Thread Dump...\n", sig)
			stacklen := runtime.Stack(buf, true)
			logger.Warn("\n\n%s\n\n", buf[:stacklen])
			logger.Warn("Thread dump complete.\n")
		}
	}()
}

// insert the netfilter queue rules for packetd
func insertRules() {
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		logger.Err("Error determining directory: %s\n", err.Error())
		return
	}
	home, ok := os.LookupEnv("PACKETD_HOME")
	if ok && home != "" {
		dir = home
	}
	syscmd.SystemCommand(dir+"/"+rulesScript, []string{})
}

// remove the netfilter queue rules for packetd
func removeRules() {
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		logger.Err("Error determining directory: %s\n", err.Error())
		return
	}
	home, ok := os.LookupEnv("PACKETD_HOME")
	if ok && home != "" {
		dir = home
	}
	syscmd.SystemCommand(dir+"/"+rulesScript, []string{"-r"})
}

// prints some basic stats about packetd
func printStats() {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	logger.Info("Memory Stats:\n")
	logger.Info("Memory Alloc: %d\n", mem.Alloc)
	logger.Info("Memory TotalAlloc: %d\n", mem.TotalAlloc)
	logger.Info("Memory HeapAlloc: %d\n", mem.HeapAlloc)
	logger.Info("Memory HeapSys: %d\n", mem.HeapSys)

	logger.Info("Reports EventsLogged: %d\n", reports.EventsLogged)
	stats, err := getProcStats()
	if err == nil {
		for _, line := range strings.Split(stats, "\n") {
			if line != "" {
				logger.Info("%s\n", line)
			}
		}
	} else {
		logger.Warn("Failed to read stats: %v\n", err)
	}
}

func getProcStats() (string, error) {
	file, err := os.OpenFile("/proc/"+strconv.Itoa(os.Getpid())+"/status", os.O_RDONLY, 0660)
	if err != nil {
		return "", err
	}

	defer file.Close()

	var interesting = ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		re, err := regexp.Compile("[[:space:]]+")
		if err != nil {
			return "", nil
		}
		line = re.ReplaceAllString(line, " ")

		if strings.HasPrefix(line, "Rss") {
			interesting += line + "\n"
		}
		if strings.HasPrefix(line, "Threads") {
			interesting += line + "\n"
		}
	}
	return interesting, nil
}

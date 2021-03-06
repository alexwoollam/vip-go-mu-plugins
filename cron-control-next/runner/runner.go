package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

type siteInfo struct {
	Multisite int
	Siteurl   string
	Disabled  int
}

type site struct {
	URL string
}

type event struct {
	URL       string
	Timestamp int
	Action    string
	Instance  string
}

var (
	wpCliPath string
	wpNetwork int
	wpPath    string

	numGetWorkers int
	numRunWorkers int

	getEventsInterval int

	heartbeatInt int64

	disabledLoopCount    uint64
	eventRunErrCount     uint64
	eventRunSuccessCount uint64

	logger  *log.Logger
	logDest string
	debug   bool

	gRestart                bool
	gEventRetrieversRunning []bool
	gEventWorkersRunning    []bool
	gSiteRetrieverRunning   bool
	gRandomDeltaMap         map[string]int64
)

const getEventsBreakSec time.Duration = 1 * time.Second
const runEventsBreakSec int64 = 10

func init() {
	flag.StringVar(&wpCliPath, "cli", "/usr/local/bin/wp", "Path to WP-CLI binary")
	flag.IntVar(&wpNetwork, "network", 0, "WordPress network ID, `0` to disable")
	flag.StringVar(&wpPath, "wp", "/var/www/html", "Path to WordPress installation")
	flag.IntVar(&numGetWorkers, "workers-get", 1, "Number of workers to retrieve events")
	flag.IntVar(&numRunWorkers, "workers-run", 5, "Number of workers to run events")
	flag.IntVar(&getEventsInterval, "get-events-interval", 60, "Seconds between event retrieval")
	flag.Int64Var(&heartbeatInt, "heartbeat", 60, "Heartbeat interval in seconds")
	flag.StringVar(&logDest, "log", "os.Stdout", "Log path, omit to log to Stdout")
	flag.BoolVar(&debug, "debug", false, "Include additional log data for debugging")
	flag.Parse()

	setUpLogger()

	// TODO: Should check for wp-config.php instead?
	validatePath(&wpCliPath, "WP-CLI path")
	validatePath(&wpPath, "WordPress path")

	gRandomDeltaMap = make(map[string]int64)
}

func main() {
	logger.Printf("Starting with %d event-retreival worker(s) and %d event worker(s)", numGetWorkers, numRunWorkers)
	logger.Printf("Retrieving events every %d seconds", getEventsInterval)
	go setupSignalHandler()

	sites := make(chan site)
	events := make(chan event)

	gEventRetrieversRunning = make([]bool, numGetWorkers)
	gEventWorkersRunning = make([]bool, numRunWorkers)

	go spawnEventRetrievers(sites, events)
	go spawnEventWorkers(events)
	go retrieveSitesPeriodically(sites)

	heartbeat(sites, events)
}

func spawnEventRetrievers(sites <-chan site, queue chan<- event) {
	for w := 1; w <= numGetWorkers; w++ {
		go queueSiteEvents(w, sites, queue)
	}
}

func spawnEventWorkers(queue <-chan event) {
	workerEvents := make(chan event)

	for w := 1; w <= numRunWorkers; w++ {
		go runEvents(w, workerEvents)
	}

	for event := range queue {
		workerEvents <- event
	}

	close(workerEvents)
}

func retrieveSitesPeriodically(sites chan<- site) {
	gSiteRetrieverRunning = true

	for {
		waitForEpoch("retrieveSitesPeriodically", int64(getEventsInterval))
		if gRestart {
			logger.Println("exiting site retriever")
			break
		}
		siteList, err := getSites()
		if err != nil {
			continue
		}

		for _, site := range siteList {
			sites <- site
		}
	}

	gSiteRetrieverRunning = false
}

func heartbeat(sites chan<- site, queue chan<- event) {
	if heartbeatInt == 0 {
		logger.Println("heartbeat disabled")
		for {
			waitForEpoch("heartbeat", 60)
			if gRestart {
				logger.Println("exiting heartbeat routine")
				break
			}
		}
		return
	}

	for {
		waitForEpoch("heartbeat", heartbeatInt)
		if gRestart {
			logger.Println("exiting heartbeat routine")
			break
		}
		successCount, errCount := atomic.LoadUint64(&eventRunSuccessCount), atomic.LoadUint64(&eventRunErrCount)
		atomic.SwapUint64(&eventRunSuccessCount, 0)
		atomic.SwapUint64(&eventRunErrCount, 0)
		logger.Printf("<heartbeat eventsSucceededSinceLast=%d eventsErroredSinceLast=%d>", successCount, errCount)
	}

	var StillRunning bool
	for {
		StillRunning = false
		for workerID, r := range gEventRetrieversRunning {
			if r {
				logger.Printf("event retriever ID %d still running\n", workerID+1)
				logger.Printf("sending empty site object for worker %d\n", workerID+1)
				sites <- site{}
				StillRunning = true
			}
		}
		for workerID, r := range gEventWorkersRunning {
			if r {
				logger.Printf("event worker ID %d still running\n", workerID+1)
				logger.Printf("sending empty event for worker %d\n", workerID+1)
				queue <- event{}
				StillRunning = true
			}
		}
		if StillRunning {
			logger.Println("worker(s) still running, waiting")
			time.Sleep(time.Duration(3) * time.Second)
			continue
		}
		logger.Println(".:sayonara:.")
		os.Exit(0)
	}
}

func getSites() ([]site, error) {
	siteInfo, err := getInstanceInfo()
	if err != nil {
		siteInfo.Disabled = 1
	}

	if run := shouldGetSites(siteInfo.Disabled); false == run {
		return nil, err
	}

	if siteInfo.Multisite == 1 {
		sites, err := getMultisiteSites()
		if err != nil {
			sites = nil
		}

		return sites, err
	}

	// Mock for single site
	sites := make([]site, 0)
	sites = append(sites, site{URL: siteInfo.Siteurl})

	return sites, nil
}

func getInstanceInfo() (siteInfo, error) {
	raw, err := runWpCliCmd([]string{"cron-control", "orchestrate", "runner-only", "get-info", "--format=json"})
	if err != nil {
		return siteInfo{}, err
	}

	jsonRes := make([]siteInfo, 0)
	if err = json.Unmarshal([]byte(raw), &jsonRes); err != nil {
		if debug {
			logger.Println(fmt.Sprintf("%+v", err))
		}

		return siteInfo{}, err
	}

	return jsonRes[0], nil
}

func shouldGetSites(disabled int) bool {
	if disabled == 0 {
		atomic.SwapUint64(&disabledLoopCount, 0)
		return true
	}

	disabledCount, now := atomic.LoadUint64(&disabledLoopCount), time.Now()
	disabledSleep := time.Minute * 3 * time.Duration(disabledCount)
	disabledSleepSeconds := int64(disabledSleep) / 1000 / 1000 / 1000

	if disabled > 1 && (now.Unix()+disabledSleepSeconds) > int64(disabled) {
		atomic.SwapUint64(&disabledLoopCount, 0)
	} else if disabledSleep > time.Hour {
		atomic.SwapUint64(&disabledLoopCount, 0)
	} else {
		atomic.AddUint64(&disabledLoopCount, 1)
	}

	if disabledSleep > 0 {
		if debug {
			logger.Printf("Automatic execution disabled, sleeping for an additional %d minutes", disabledSleepSeconds/60)
		}

		time.Sleep(disabledSleep)
	} else if debug {
		logger.Println("Automatic execution disabled")
	}

	return false
}

func getMultisiteSites() ([]site, error) {
	raw, err := runWpCliCmd([]string{"site", "list", "--fields=url", "--archived=false", "--deleted=false", "--spam=false", "--format=json"})
	if err != nil {
		return nil, err
	}

	jsonRes := make([]site, 0)
	if err = json.Unmarshal([]byte(raw), &jsonRes); err != nil {
		if debug {
			logger.Println(fmt.Sprintf("%+v", err))
		}

		return nil, err
	}

	// Shuffle site order so that none are favored
	for i := range jsonRes {
		j := rand.Intn(i + 1)
		jsonRes[i], jsonRes[j] = jsonRes[j], jsonRes[i]
	}

	return jsonRes, nil
}

func queueSiteEvents(workerID int, sites <-chan site, queue chan<- event) {
	gEventRetrieversRunning[workerID-1] = true
	logger.Printf("started retriever %d\n", workerID)

OuterLoop:
	for site := range sites {
		if gRestart {
			logger.Printf("exiting event retriever ID %d\n", workerID)
			break
		}
		if debug {
			logger.Printf("getEvents-%d processing %s", workerID, site.URL)
		}

		events, err := getSiteEvents(site.URL)
		if err == nil && len(events) > 0 {
			for _, event := range events {
				if gRestart {
					break OuterLoop
				}
				event.URL = site.URL
				queue <- event
			}
		}
		time.Sleep(getEventsBreakSec)
	}
	// Mark this event retriever as not running for graceful exit
	gEventRetrieversRunning[workerID-1] = false
}

func getSiteEvents(site string) ([]event, error) {
	raw, err := runWpCliCmd([]string{"cron-control", "orchestrate", "runner-only", "list-due-batch", fmt.Sprintf("--url=%s", site), "--format=json"})
	if err != nil {
		return nil, err
	}

	siteEvents := make([]event, 0)
	if err = json.Unmarshal([]byte(raw), &siteEvents); err != nil {
		if debug {
			logger.Println(fmt.Sprintf("%+v", err))
		}

		return nil, err
	}

	return siteEvents, nil
}

func runEvents(workerID int, events <-chan event) {
	gEventWorkersRunning[workerID-1] = true
	logger.Printf("started event worker %d\n", workerID)

	for event := range events {
		if gRestart {
			logger.Printf("exiting event worker ID %d\n", workerID)
			break
		}
		if now := time.Now(); event.Timestamp > int(now.Unix()) {
			if debug {
				logger.Printf("runEvents-%d skipping premature job %d|%s|%s for %s", workerID, event.Timestamp, event.Action, event.Instance, event.URL)
			}

			continue
		}

		subcommand := []string{"cron-control", "orchestrate", "runner-only", "run", fmt.Sprintf("--timestamp=%d", event.Timestamp),
			fmt.Sprintf("--action=%s", event.Action), fmt.Sprintf("--instance=%s", event.Instance), fmt.Sprintf("--url=%s", event.URL)}

		_, err := runWpCliCmd(subcommand)

		if err == nil {
			if heartbeatInt > 0 {
				atomic.AddUint64(&eventRunSuccessCount, 1)
			}

			if debug {
				logger.Printf("runEvents-%d finished job %d|%s|%s for %s", workerID, event.Timestamp, event.Action, event.Instance, event.URL)
			}
		} else if heartbeatInt > 0 {
			atomic.AddUint64(&eventRunErrCount, 1)
		}

		waitForEpoch("runEvents", runEventsBreakSec)
		if gRestart {
			logger.Printf("exiting event worker ID %d\n", workerID)
			break
		}

	}

	// Mark this event worker as not running for graceful exit
	gEventWorkersRunning[workerID-1] = false
}

func runWpCliCmd(subcommand []string) (string, error) {
	// `--quiet`` included to prevent WP-CLI commands from generating invalid JSON
	subcommand = append(subcommand, "--allow-root", "--quiet", fmt.Sprintf("--path=%s", wpPath))
	if wpNetwork > 0 {
		subcommand = append(subcommand, fmt.Sprintf("--network=%d", wpNetwork))
	}

	wpCli := exec.Command(wpCliPath, subcommand...)
	wpOut, err := wpCli.CombinedOutput()
	wpOutStr := string(wpOut)

	if err != nil {
		if debug {
			logger.Printf("%s - %s", err, wpOutStr)
			logger.Println(fmt.Sprintf("%+v", subcommand))
		}

		return wpOutStr, err
	}

	return wpOutStr, nil
}

func setUpLogger() {
	logOpts := log.Ldate | log.Ltime | log.LUTC | log.Lshortfile

	if logDest == "os.Stdout" {
		logger = log.New(os.Stdout, "DEBUG: ", logOpts)
	} else {
		path, err := filepath.Abs(logDest)
		if err != nil {
			logger.Fatal(err)
		}

		logFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			log.Fatal(err)
		}

		logger = log.New(logFile, "", logOpts)
	}
}

func validatePath(path *string, label string) {
	if len(*path) > 1 {
		var err error
		*path, err = filepath.Abs(*path)

		if err != nil {
			fmt.Printf("Error for %s: %s\n", label, err.Error())
			os.Exit(3)
		}

		if _, err = os.Stat(*path); os.IsNotExist(err) {
			fmt.Printf("Error for %s: '%s' does not exist\n", label, *path)
			usage()
		}
	} else {
		fmt.Printf("Empty path provided for %s\n", label)
		usage()
	}
}

func usage() {
	flag.Usage()
	os.Exit(3)
}

func waitForEpoch(whom string, epoch_sec int64) {
	tEpochNano := epoch_sec * time.Second.Nanoseconds()
	tEpochDelta := tEpochNano - (time.Now().UnixNano() % tEpochNano)
	if tEpochDelta < 1*time.Second.Nanoseconds() {
		tEpochDelta += epoch_sec * time.Second.Nanoseconds()
	}

	// We need to offset each epoch wait by a fixed random value to prevent
	// all Cron Runners having their epochs at exactly the same time.
	_, found := gRandomDeltaMap[whom]
	if !found {
		rand.Seed(time.Now().UnixNano() + epoch_sec)
		gRandomDeltaMap[whom] = rand.Int63n(tEpochNano)
	}

	tNextEpoch := time.Now().UnixNano() + tEpochDelta + gRandomDeltaMap[whom]

	// Sleep in 3sec intervals by default, less if we are running out of time
	tMaxDelta := 3 * time.Second.Nanoseconds()
	tDelta := tMaxDelta

	for i := tDelta; time.Now().UnixNano() < tNextEpoch; i += tDelta {
		if i > tEpochNano*2 {
			// if we ever loop here for more than 2 full epochs, bail out
			logger.Printf("Error in the epoch wait loop for %s\n", whom)
			break
		}
		if gRestart {
			return
		}
		tDelta = tNextEpoch - time.Now().UnixNano()
		if tDelta > tMaxDelta {
			tDelta = tMaxDelta
		}
		time.Sleep(time.Duration(tDelta))
	}
}

func setupSignalHandler() {
	sigChan := make(chan os.Signal)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	for {
		select {
		case sig := <-sigChan:
			logger.Printf("caught termination signal %s, scheduling shutdown\n", sig)
			gRestart = true
		}
	}
}

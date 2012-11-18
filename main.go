/*
   Copyright 2009-2012 Phil Pennock

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package sks_spider

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"runtime"
	"sync"
	"time"
)

var (
	flSpiderStartHost    = flag.String("spider-start-host", "sks-peer.spodhuis.org", "Host to query to start things rolling")
	flListen             = flag.String("listen", "localhost:8001", "port to listen on with web-server")
	flMaintEmail         = flag.String("maint-email", "webmaster@spodhuis.org", "Email address of local maintainer")
	flHostname           = flag.String("hostname", "sks.spodhuis.org", "Hostname to use in generated pages")
	flSksMembershipFile  = flag.String("sks-membership-file", "/var/sks/membership", "SKS Membership file")
	flSksPortRecon       = flag.Int("sks-port-recon", 11370, "Default SKS recon port")
	flSksPortHkp         = flag.Int("sks-port-hkp", 11371, "Default SKS HKP port")
	flTimeoutStatsFetch  = flag.Int("timeout-stats-fetch", 30, "Timeout for fetching stats from a remote server")
	flCountriesZone      = flag.String("countries-zone", "zz.countries.nerd.dk.", "DNS zone for determining IP locations")
	flKeysSanityMin      = flag.Int("keys-sanity-min", 3100000, "Minimum number of keys that's sane, or we're broken")
	flKeysDailyJitter    = flag.Int("keys-daily-jitter", 500, "Max daily jitter in key count")
	flScanIntervalSecs   = flag.Int("scan-interval", 3600*8, "How often to trigger a scan")
	flScanIntervalJitter = flag.Int("scan-interval-jitter", 120, "Jitter in scan interval")
	flLogFile            = flag.String("log-file", "sksdaemon.log", "Where to write logfiles")
	flLogStdout          = flag.Bool("log-stdout", false, "Log to stdout instead of log-file")
	flJsonDump           = flag.String("json-dump", "", "File to dump JSON of spidered hosts to")
	flJsonLoad           = flag.String("json-load", "", "File to load JSON hosts from instead of spidering")
)

var serverHeadersNative = map[string]bool{
	"sks_www": true,
	"gnuks":   true,
}
var defaultSoftware = "SKS"

// People put dumb things in their membership files
var blacklistedQueryHosts = []string{
	"localhost",
	"127.0.0.1",
	"::1",
}

var Log *log.Logger

func setupLogging() {
	if *flLogStdout {
		Log = log.New(os.Stdout, "", log.LstdFlags|log.Lshortfile)
		return
	}
	fh, err := os.OpenFile(*flLogFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to open logfile \"%s\": %s\n", *flLogFile, err)
		os.Exit(1)
	}
	Log = log.New(fh, "", log.LstdFlags|log.Lshortfile)
}

type PersistedHostInfo struct {
	HostMap      HostMap
	AliasMap     AliasMap
	IPCountryMap IPCountryMap
	Sorted       []string
	DepthSorted  []string
	Graph        *HostGraph
}

var (
	currentHostInfo    *PersistedHostInfo
	currentHostMapLock sync.Mutex
)

func GetCurrentPersisted() *PersistedHostInfo {
	currentHostMapLock.Lock()
	defer currentHostMapLock.Unlock()
	return currentHostInfo
}

func GetCurrentHosts() HostMap {
	currentHostMapLock.Lock()
	defer currentHostMapLock.Unlock()
	if currentHostInfo == nil {
		return nil
	}
	return currentHostInfo.HostMap
}

func GetCurrentHostlist() []string {
	currentHostMapLock.Lock()
	defer currentHostMapLock.Unlock()
	if currentHostInfo == nil {
		return nil
	}
	return currentHostInfo.Sorted
}

func SetCurrentPersisted(p *PersistedHostInfo) {
	p.LogInformation()
	currentHostMapLock.Lock()
	defer currentHostMapLock.Unlock()
	currentHostInfo = p
}

func normaliseMeshAndSet(spider *Spider, dumpJson bool) {
	go func(s *Spider) {
		persisted := GeneratePersistedInformation(s)
		SetCurrentPersisted(persisted)
		runtime.GC()
		if dumpJson && *flJsonDump != "" {
			Log.Printf("Saving JSON to \"%s\"", *flJsonDump)
			err := persisted.HostMap.DumpJSONToFile(*flJsonDump)
			if err != nil {
				Log.Printf("Error saving JSON to \"%s\": %s", *flJsonDump, err)
				// continue anyway
			}
			runtime.GC()
		}
	}(spider)
}

func respiderPeriodically() {
	for {
		var delay time.Duration = time.Duration(*flScanIntervalSecs) * time.Second
		if *flScanIntervalJitter > 0 {
			jitter := rand.Int63n(int64(*flScanIntervalJitter) * int64(time.Second))
			jitter -= int64(*flScanIntervalJitter) * int64(time.Second) / 2
			delay += time.Duration(jitter)
		}
		minDelay := time.Minute * 30
		if delay < minDelay {
			Log.Printf("respider period too low, capping %d up to %d", delay, minDelay)
			delay = minDelay
		}
		Log.Printf("Sleeping %s before next respider", delay)
		time.Sleep(delay)
		Log.Printf("Awoken!  Time to spider.")
		spider := StartSpider()
		spider.AddHost(*flSpiderStartHost, 0)
		spider.Wait()
		spider.Terminate()
		normaliseMeshAndSet(spider, false)
	}
}

var httpServing sync.WaitGroup

func startHttpServing() {
	Log.Printf("Will Listen on <%s>", *flListen)
	server := setupHttpServer(*flListen)
	err := server.ListenAndServe()
	if err != nil {
		Log.Printf("ListenAndServe(%s): %s", *flListen, err)
	}
	httpServing.Done()
}

func Main() {
	flag.Parse()

	if *flScanIntervalJitter < 0 {
		fmt.Fprintf(os.Stderr, "Bad jitter, must be >= 0 [got: %d]\n", *flScanIntervalJitter)
		os.Exit(1)
	}

	setupLogging()
	Log.Printf("started")

	httpServing.Add(1)
	go startHttpServing()

	if *flJsonLoad != "" {
		Log.Printf("Loading hosts from \"%s\" instead of spidering", *flJsonLoad)
		hostmap, err := LoadJSONFromFile(*flJsonLoad)
		if err != nil {
			Log.Fatalf("Failed to load JSON from \"%s\": %s", *flJsonLoad, err)
		}
		Log.Printf("Loaded %d hosts from JSON", len(hostmap))
		hostnames := GenerateHostlistSorted(hostmap)
		countryMap := GetFreshCountryForHostmap(hostmap)
		aliasMap := GetAliasMapForHostmap(hostmap)
		SetCurrentPersisted(&PersistedHostInfo{
			HostMap:      hostmap,
			AliasMap:     aliasMap,
			IPCountryMap: countryMap,
			Sorted:       hostnames,
			DepthSorted:  GenerateDepthSorted(hostmap),
			Graph:        GenerateGraph(hostnames, hostmap, aliasMap),
		})
	} else {
		spider := StartSpider()
		spider.AddHost(*flSpiderStartHost, 0)
		spider.Wait()
		spider.Terminate()
		Log.Printf("Spidering complete")
		normaliseMeshAndSet(spider, true)
		go respiderPeriodically()
	}

	httpServing.Wait()
}

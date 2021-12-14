package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	_ "io"
	"io/ioutil"
	_ "log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/hawkinsw/goresponsiveness/ma"
	"github.com/hawkinsw/goresponsiveness/mc"
	"github.com/hawkinsw/goresponsiveness/timeoutat"
	"github.com/hawkinsw/goresponsiveness/utilities"
)

type ConfigUrls struct {
	SmallUrl  string `json:"small_https_download_url"`
	LargeUrl  string `json:"large_https_download_url"`
	UploadUrl string `json:"https_upload_url"`
}

type Config struct {
	Version int
	Urls    ConfigUrls `json:"urls"`
}

func (c *Config) String() string {
	return fmt.Sprintf("Version: %d\nSmall URL: %s\nLarge URL: %s\nUpload URL: %s", c.Version, c.Urls.SmallUrl, c.Urls.LargeUrl, c.Urls.UploadUrl)
}

func (c *Config) IsValid() error {
	if parsedUrl, err := url.ParseRequestURI(c.Urls.LargeUrl); err != nil || parsedUrl.Scheme != "https" {
		return fmt.Errorf("Configuration url large_https_download_url is invalid: %s", utilities.Conditional(len(c.Urls.LargeUrl) != 0, c.Urls.LargeUrl, "Missing"))
	}
	if parsedUrl, err := url.ParseRequestURI(c.Urls.SmallUrl); err != nil || parsedUrl.Scheme != "https" {
		return fmt.Errorf("Configuration url small_https_download_url is invalid: %s", utilities.Conditional(len(c.Urls.SmallUrl) != 0, c.Urls.SmallUrl, "Missing"))
	}
	if parsedUrl, err := url.ParseRequestURI(c.Urls.UploadUrl); err != nil || parsedUrl.Scheme != "https" {
		return fmt.Errorf("Configuration url https_upload_url is invalid: %s", utilities.Conditional(len(c.Urls.UploadUrl) != 0, c.Urls.UploadUrl, "Missing"))
	}
	return nil
}

func toMBs(bytes float64) float64 {
	return float64(bytes) / float64(1024*1024)
}

var (
	// Variables to hold CLI arguments.
	configHost = flag.String("config", "networkquality.example.com", "name/IP of responsiveness configuration server.")
	configPort = flag.Int("port", 4043, "port number on which to access responsiveness configuration server.")
	debug      = flag.Bool("debug", false, "Enable debugging.")
	timeout    = flag.Int("timeout", 20, "Maximum time to spend measuring.")
)

func addFlows(ctx context.Context, toAdd uint64, mcs *[]mc.MeasurableConnection, mcsPreviousTransferred *[]uint64, lbcGenerator func() mc.MeasurableConnection, debug bool) {
	for i := uint64(0); i < toAdd; i++ {
		//mcs[i] = &mc.LoadBearingUpload{Path: config.Urls.UploadUrl}
		*mcs = append(*mcs, lbcGenerator())
		*mcsPreviousTransferred = append(*mcsPreviousTransferred, 0)
		if !(*mcs)[len(*mcs)-1].Start(ctx, debug) {
			fmt.Printf("Error starting %dth MC!\n", i)
			return
		}
	}
}

type SaturationResult struct {
	RateBps   float64
	FlowCount uint64
}

func saturate(ctx context.Context, saturated chan<- SaturationResult, lbcGenerator func() mc.MeasurableConnection, debug bool) {
	mcs := make([]mc.MeasurableConnection, 0)
	mcsPreviousTransferred := make([]uint64, 0)

	// Create 4 load bearing connections
	addFlows(ctx, 4, &mcs, &mcsPreviousTransferred, lbcGenerator, debug)

	previousFlowIncreaseIteration := uint64(0)
	previousMovingAverage := float64(0)
	movingAverage := ma.NewMovingAverage(4)
	movingAverageAverage := ma.NewMovingAverage(4)

	for currentIteration := uint64(0); true; currentIteration++ {

		// If we are cancelled, then stop.
		if ctx.Err() != nil {
			return
		}

		// At each 1-second interval
		time.Sleep(time.Second)

		// Compute "instantaneous aggregate" goodput which is the number of bytes transferred within the last second.
		totalTransfer := uint64(0)
		for i := range mcs {
			previousTransferred := mcsPreviousTransferred[i]
			currentTransferred := mcs[i].Transferred()
			totalTransfer += (currentTransferred - previousTransferred)
			mcsPreviousTransferred[i] = currentTransferred
		}

		// Compute a moving average of the last 4 "instantaneous aggregate goodput" measurements
		movingAverage.AddMeasurement(float64(totalTransfer))
		currentMovingAverage := movingAverage.CalculateAverage()
		movingAverageAverage.AddMeasurement(currentMovingAverage)
		movingAverageDelta := utilities.SignedPercentDifference(currentMovingAverage, previousMovingAverage)
		previousMovingAverage = currentMovingAverage

		if debug {
			fmt.Printf("Instantaneous goodput: %f MB.\n", toMBs(float64(totalTransfer)))
			fmt.Printf("Moving average: %f MB.\n", toMBs(currentMovingAverage))
			fmt.Printf("Moving average delta: %f.\n", movingAverageDelta)
		}

		// If moving average > "previous" moving average + 5%:
		if currentIteration == 0 || movingAverageDelta > float64(5) {
			// Network did not yet reach saturation. If no flows added within the last 4 seconds, add 4 more flows
			if (currentIteration - previousFlowIncreaseIteration) > 4 {
				if debug {
					fmt.Printf("Adding flows because we are unsaturated and waited a while.\n")
				}
				addFlows(ctx, 4, &mcs, &mcsPreviousTransferred, lbcGenerator, debug)
				previousFlowIncreaseIteration = currentIteration
			} else {
				if debug {
					fmt.Printf("We are unsaturated, but it still too early to add anything.\n")
				}
			}
		} else { // Else, network reached saturation for the current flow count.
			// If new flows added and for 4 seconds the moving average throughput did not change: network reached stable saturation
			if (currentIteration-previousFlowIncreaseIteration) < 4 && movingAverageAverage.ConsistentWithin(float64(4)) {
				if debug {
					fmt.Printf("New flows added within the last four seconds and the moving-average average is consistent!\n")
				}
				break
			} else {
				// Else, add four more flows
				if debug {
					fmt.Printf("New flows to add to try to increase our saturation!\n")
				}
				addFlows(ctx, 4, &mcs, &mcsPreviousTransferred, lbcGenerator, debug)
				previousFlowIncreaseIteration = currentIteration
			}
		}

	}
	saturated <- SaturationResult{RateBps: movingAverage.CalculateAverage(), FlowCount: uint64(len(mcs))}
}

func main() {
	flag.Parse()

	timeoutDuration := time.Second * time.Duration(*timeout)

	configHostPort := fmt.Sprintf("%s:%d", *configHost, *configPort)
	configUrl := fmt.Sprintf("https://%s/config", configHostPort)

	configClient := &http.Client{}
	resp, err := configClient.Get(configUrl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Could not connect to configuration host %s: %v\n", configHostPort, err)
		return
	}

	jsonConfig, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Could not read configuration content downloaded from %s: %v\n", configUrl, err)
		return
	}

	var config Config
	err = json.Unmarshal(jsonConfig, &config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Could not parse configuration returned from %s: %v\n", configUrl, err)
		return
	}

	// TODO: Make sure that all configuration values are present and accounted for!

	if err := config.IsValid(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Invalid configuration returned from %s: %v\n", configUrl, err)
		return
	}

	if *debug {
		fmt.Printf("Configuration: %s\n", &config)
	}

	operatingCtx, cancelOperatingCtx := context.WithCancel(context.Background())

	uploadSaturationChannel := make(chan SaturationResult)
	downloadSaturationChannel := make(chan SaturationResult)

	timeoutChannel := timeoutat.TimeoutAt(operatingCtx, time.Now().Add(timeoutDuration), *debug)

	generate_lbd := func() mc.MeasurableConnection {
		return &mc.LoadBearingDownload{Path: config.Urls.LargeUrl}
	}
	generate_lbu := func() mc.MeasurableConnection {
		return &mc.LoadBearingUpload{Path: config.Urls.UploadUrl}
	}

	go saturate(operatingCtx, downloadSaturationChannel, generate_lbd, *debug)
	go saturate(operatingCtx, uploadSaturationChannel, generate_lbu, *debug)

	saturation_timeout := false
	upload_saturated := false
	download_saturated := false

	for !saturation_timeout && !(upload_saturated && download_saturated) {
		select {
		case saturatedDownloadRate := <-downloadSaturationChannel:
			{
				download_saturated = true
				if *debug {
					fmt.Printf("################# download is saturated (%fMBps, %d flows)!\n", toMBs(saturatedDownloadRate.RateBps), saturatedDownloadRate.FlowCount)
				}
			}
		case saturatedUploadRate := <-uploadSaturationChannel:
			{
				upload_saturated = true
				if *debug {
					fmt.Printf("################# upload is saturated (%fMBps, %d flows)!\n", toMBs(saturatedUploadRate.RateBps), saturatedUploadRate.FlowCount)
				}
			}
		case <-timeoutChannel:
			{
				saturation_timeout = true
				if *debug {
					fmt.Printf("################# timeout reaching saturation!\n")
				}
			}
		}
	}

	if saturation_timeout {
		cancelOperatingCtx()
		fmt.Fprintf(os.Stderr, "Error: Did not reach upload/download saturation in maximum time of %v\n.", timeoutDuration)
		return
	}

	time.Sleep(10 * time.Second)

	cancelOperatingCtx()

	time.Sleep(4 * time.Second)
}

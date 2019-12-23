package main

import (
	"flag"
	"math/rand"
	"net/http"
	"time"

	"github.com/PubMatic-OpenWrap/prebid-server/config"
	"github.com/PubMatic-OpenWrap/prebid-server/currencies"
	pbc "github.com/PubMatic-OpenWrap/prebid-server/prebid_cache_client"
	"github.com/PubMatic-OpenWrap/prebid-server/router"
	"github.com/PubMatic-OpenWrap/prebid-server/server"

	"github.com/golang/glog"
	"github.com/spf13/viper"
)

// Rev holds binary revision string
// Set manually at build time using:
//    go build -ldflags "-X main.Rev=`git rev-parse --short HEAD`"
// Populated automatically at build / release time via .travis.yml
//   `gox -os="linux" -arch="386" -output="{{.Dir}}_{{.OS}}_{{.Arch}}" -ldflags "-X main.Rev=`git rev-parse --short HEAD`" -verbose ./...;`
// See issue #559
var Rev string

func init() {
	rand.Seed(time.Now().UnixNano())
}

func main() {
	flag.Parse() // required for glog flags and testing package flags

	cfg, err := loadConfig()
	if err != nil {
		glog.Fatalf("Configuration could not be loaded or did not pass validation: %v", err)
	}

	err = serve(Rev, cfg)
	if err != nil {
		glog.Errorf("prebid-server failed: %v", err)
	}
}

func loadConfig() (*config.Configuration, error) {
	v := viper.New()
	config.SetupViper(v, "pbs") // filke = filename
	return config.New(v)
}

func serve(revision string, cfg *config.Configuration) error {
	fetchingInterval := time.Duration(cfg.CurrencyConverter.FetchIntervalSeconds) * time.Second
	currencyConverter := currencies.NewRateConverter(&http.Client{}, cfg.CurrencyConverter.FetchURL, fetchingInterval)

	r, err := router.New(cfg, currencyConverter)
	if err != nil {
		return err
	}

	pbc.InitPrebidCache(cfg.CacheURL.GetBaseURL())

	corsRouter := router.SupportCORS(r)
	server.Listen(cfg, router.NoCache{Handler: corsRouter}, router.Admin(revision, currencyConverter), r.MetricsEngine)

	r.Shutdown()
	return nil
}

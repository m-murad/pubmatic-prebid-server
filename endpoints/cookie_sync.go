package endpoints

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/PubMatic-OpenWrap/prebid-server/analytics"
	"github.com/PubMatic-OpenWrap/prebid-server/config"
	"github.com/PubMatic-OpenWrap/prebid-server/gdpr"
	"github.com/PubMatic-OpenWrap/prebid-server/openrtb_ext"
	"github.com/PubMatic-OpenWrap/prebid-server/pbsmetrics"
	"github.com/PubMatic-OpenWrap/prebid-server/privacy"
	"github.com/PubMatic-OpenWrap/prebid-server/privacy/ccpa"
	gdprPolicy "github.com/PubMatic-OpenWrap/prebid-server/privacy/gdpr"
	"github.com/PubMatic-OpenWrap/prebid-server/usersync"
	"github.com/buger/jsonparser"
	"github.com/golang/glog"
	"github.com/julienschmidt/httprouter"
)

var secureFlagRegex = regexp.MustCompile(`(%7B|{)SecParam(%7D|})`)

func NewCookieSyncEndpoint(syncers map[openrtb_ext.BidderName]usersync.Usersyncer, cfg *config.Configuration, syncPermissions gdpr.Permissions, metrics pbsmetrics.MetricsEngine, pbsAnalytics analytics.PBSAnalyticsModule) httprouter.Handle {
	deps := &cookieSyncDeps{
		syncers:         syncers,
		hostCookie:      &cfg.HostCookie,
		gDPR:            &cfg.GDPR,
		syncPermissions: syncPermissions,
		metrics:         metrics,
		pbsAnalytics:    pbsAnalytics,
		enforceCCPA:     cfg.CCPA.Enforce,
	}
	return deps.Endpoint
}

type cookieSyncDeps struct {
	syncers         map[openrtb_ext.BidderName]usersync.Usersyncer
	hostCookie      *config.HostCookie
	gDPR            *config.GDPR
	syncPermissions gdpr.Permissions
	metrics         pbsmetrics.MetricsEngine
	pbsAnalytics    analytics.PBSAnalyticsModule
	enforceCCPA     bool
}

func (deps *cookieSyncDeps) Endpoint(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {

	//CookieSyncObject makes a log of requests and responses to  /cookie_sync endpoint
	co := analytics.CookieSyncObject{
		Status:       http.StatusOK,
		Errors:       make([]error, 0),
		BidderStatus: make([]*usersync.CookieSyncBidders, 0),
	}

	defer deps.pbsAnalytics.LogCookieSyncObject(&co)

	deps.metrics.RecordCookieSync()
	userSyncCookie := usersync.ParsePBSCookieFromRequest(r, deps.hostCookie)
	if !userSyncCookie.AllowSyncs() {
		http.Error(w, "User has opted out", http.StatusUnauthorized)
		co.Status = http.StatusUnauthorized
		co.Errors = append(co.Errors, fmt.Errorf("user has opted out"))
		return
	}

	defer r.Body.Close()
	bodyBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		co.Status = http.StatusBadRequest
		co.Errors = append(co.Errors, errors.New("Failed to read request body"))
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	biddersJSON, err := parseBidders(bodyBytes)
	if err != nil {
		co.Status = http.StatusBadRequest
		co.Errors = append(co.Errors, errors.New("Failed to check request.bidders in request body. Was your JSON well-formed?"))
		http.Error(w, "Failed to check request.bidders in request body. Was your JSON well-formed?", http.StatusBadRequest)
		return
	}

	parsedReq := &cookieSyncRequest{}
	if err := parseRequest(parsedReq, bodyBytes, deps.gDPR.UsersyncIfAmbiguous); err != nil {
		co.Status = http.StatusBadRequest
		co.Errors = append(co.Errors, err)
		http.Error(w, co.Errors[len(co.Errors)-1].Error(), co.Status)
		return
	}

	if len(biddersJSON) == 0 {
		parsedReq.Bidders = make([]string, 0, len(deps.syncers))
		for bidder := range deps.syncers {
			parsedReq.Bidders = append(parsedReq.Bidders, string(bidder))
		}
	}
	setSiteCookie := siteCookieCheck(r.UserAgent())
	needSyncupForSameSite := false
	if setSiteCookie {
		_, err1 := r.Cookie(usersync.SameSiteCookieName)
		if err1 == http.ErrNoCookie {
			needSyncupForSameSite = true
		}
	}

	privacyPolicy := privacy.Policies{
		GDPR: gdprPolicy.Policy{
			Signal:  gdprToString(parsedReq.GDPR),
			Consent: parsedReq.Consent,
		},
		CCPA: ccpa.Policy{
			Value: parsedReq.USPrivacy,
		},
	}

	parsedReq.filterExistingSyncs(deps.syncers, userSyncCookie, needSyncupForSameSite)

	adapterSyncs := make(map[openrtb_ext.BidderName]bool)
	// assume all bidders will be privacy blocked
	for _, b := range parsedReq.Bidders {
		adapterSyncs[openrtb_ext.BidderName(b)] = true
	}
	parsedReq.filterForPrivacy(deps.syncPermissions, privacyPolicy, deps.enforceCCPA)
	// surviving bidders are not privacy blocked
	for _, b := range parsedReq.Bidders {
		adapterSyncs[openrtb_ext.BidderName(b)] = false
	}
	for b, g := range adapterSyncs {
		deps.metrics.RecordAdapterCookieSync(b, g)
	}
	parsedReq.filterToLimit()

	csResp := cookieSyncResponse{
		Status:       cookieSyncStatus(userSyncCookie.LiveSyncCount()),
		BidderStatus: make([]*usersync.CookieSyncBidders, 0, len(parsedReq.Bidders)),
	}

	//For secure = true flag on cookie
	secParam := r.URL.Query().Get("sec")
	refererHeader := r.Header.Get("Referer")
	setSecureFlag := false
	if secParam == "1" || strings.HasPrefix(refererHeader, "https") {
		setSecureFlag = true
	}

	for i := 0; i < len(parsedReq.Bidders); i++ {
		bidder := parsedReq.Bidders[i]

		//added hack to support to old wrapper versions having indexExchange as partner
		//TODO: Remove when a stable version is released
		newBidder := bidder
		if bidder == "indexExchange" {
			newBidder = "ix"
		}
		syncInfo, err := deps.syncers[openrtb_ext.BidderName(newBidder)].GetUsersyncInfo(privacyPolicy)
		if err == nil {

			syncInfo.URL = setSecureParam(syncInfo.URL, setSecureFlag)

			newSync := &usersync.CookieSyncBidders{
				BidderCode:   bidder,
				NoCookie:     true,
				UsersyncInfo: syncInfo,
			}
			csResp.BidderStatus = append(csResp.BidderStatus, newSync)
		} else {
			glog.Errorf("Failed to get usersync info for %s: %v", newBidder, err)
		}
	}

	if len(csResp.BidderStatus) > 0 {
		co.BidderStatus = append(co.BidderStatus, csResp.BidderStatus...)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(csResp)
}

func parseRequest(parsedReq *cookieSyncRequest, bodyBytes []byte, usersyncIfAmbiguous bool) error {
	if err := json.Unmarshal(bodyBytes, parsedReq); err != nil {
		return fmt.Errorf("JSON parsing failed: %s", err.Error())
	}

	if parsedReq.GDPR != nil && *parsedReq.GDPR == 1 && parsedReq.Consent == "" {
		return errors.New("gdpr_consent is required if gdpr=1")
	}
	// If GDPR is ambiguous, lets untangle it here.
	if parsedReq.GDPR == nil {
		var gdpr = new(int)
		*gdpr = 1
		if usersyncIfAmbiguous {
			*gdpr = 0
		}
		parsedReq.GDPR = gdpr
	}
	return nil
}

func gdprToString(gdpr *int) string {
	if gdpr == nil {
		return ""
	}
	return strconv.Itoa(*gdpr)
}

func parseBidders(request []byte) ([]byte, error) {
	value, valueType, _, err := jsonparser.Get(request, "bidders")
	if err == nil && valueType != jsonparser.NotExist {
		return value, nil
	} else if err != jsonparser.KeyPathNotFoundError {
		return nil, err
	}
	return nil, nil
}

func cookieSyncStatus(syncCount int) string {
	if syncCount == 0 {
		return "no_cookie"
	}
	return "ok"
}

func setSecureParam(userSyncUrl string, isSecure bool) string {
	var secParam = "0"
	if isSecure {
		secParam = "1"
	}
	syncURL := secureFlagRegex.ReplaceAllString(userSyncUrl, secParam)
	return syncURL
}

type CookieSyncReq cookieSyncRequest
type CookieSyncResp cookieSyncResponse

type cookieSyncRequest struct {
	Bidders   []string `json:"bidders"`
	GDPR      *int     `json:"gdpr"`
	Consent   string   `json:"gdpr_consent"`
	USPrivacy string   `json:"us_privacy"`
	Limit     int      `json:"limit"`
}

func (req *cookieSyncRequest) filterExistingSyncs(valid map[openrtb_ext.BidderName]usersync.Usersyncer, cookie *usersync.PBSCookie, needSyncupForSameSite bool) {
	for i := 0; i < len(req.Bidders); i++ {
		thisBidder := req.Bidders[i]
		//added hack to support to old wrapper versions having indexExchange as partner
		//TODO: Remove when a stable version is released
		if thisBidder == "indexExchange" {
			thisBidder = "ix"
		}
		if syncer, isValid := valid[openrtb_ext.BidderName(thisBidder)]; !isValid || (cookie.HasLiveSync(syncer.FamilyName()) && !needSyncupForSameSite) {
			req.Bidders = append(req.Bidders[:i], req.Bidders[i+1:]...)
			i--
		}
	}
}

func (req *cookieSyncRequest) filterForPrivacy(permissions gdpr.Permissions, privacyPolicies privacy.Policies, enforceCCPA bool) {
	if enforceCCPA && privacyPolicies.CCPA.ShouldEnforce() {
		req.Bidders = nil
		return
	}

	if req.GDPR != nil && *req.GDPR == 0 {
		return
	}

	if allowSync, err := permissions.HostCookiesAllowed(context.Background(), req.Consent); err != nil || !allowSync {
		req.Bidders = nil
		return
	}

	for i := 0; i < len(req.Bidders); i++ {
		if allowSync, err := permissions.BidderSyncAllowed(context.Background(), openrtb_ext.BidderName(req.Bidders[i]), req.Consent); err != nil || !allowSync {
			req.Bidders = append(req.Bidders[:i], req.Bidders[i+1:]...)
			i--
		}
	}
}

// filterToLimit will enforce a max limit on cookiesyncs supplied, picking a random subset of syncs to get to the limit if over.
func (req *cookieSyncRequest) filterToLimit() {
	if req.Limit <= 0 {
		return
	}
	if req.Limit >= len(req.Bidders) {
		return
	}

	// Modified Fisher and Yates' shuffle. We don't need the bidder list shuffled, so we stop shuffling once the final values beyond limit have been set.
	// We also don't bother saving the values that should go into the entries beyond limit, as they will be discarded.
	for i := len(req.Bidders) - 1; i >= req.Limit; i-- {
		j := rand.Intn(i + 1)
		if i != j {
			req.Bidders[j] = req.Bidders[i]
			// Don't complete the swap as the new value for req.Bidders[i] will be discarded below, and will never again be accessed as part of the swapping.
		}
	}
	req.Bidders = req.Bidders[:req.Limit]
	return
}

type cookieSyncResponse struct {
	Status       string                        `json:"status"`
	BidderStatus []*usersync.CookieSyncBidders `json:"bidder_status"`
}

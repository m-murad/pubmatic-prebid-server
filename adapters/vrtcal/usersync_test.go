package vrtcal

import (
	"testing"
	"text/template"

	"github.com/PubMatic-OpenWrap/prebid-server/privacy"
	"github.com/PubMatic-OpenWrap/prebid-server/privacy/gdpr"
	"github.com/stretchr/testify/assert"
)

func TestVrtcalSyncer(t *testing.T) {
	syncURL := "http://usync-prebid.vrtcal.com/s?gdpr={{.GDPR}}&gdpr_consent={{.GDPRConsent}}"
	syncURLTemplate := template.Must(
		template.New("sync-template").Parse(syncURL),
	)

	syncer := NewVrtcalSyncer(syncURLTemplate)
	syncInfo, err := syncer.GetUsersyncInfo(privacy.Policies{
		GDPR: gdpr.Policy{
			Signal: "0",
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, "http://usync-prebid.vrtcal.com/s?gdpr=0&gdpr_consent=", syncInfo.URL)
	assert.Equal(t, "redirect", syncInfo.Type)
	assert.EqualValues(t, 0, syncer.GDPRVendorID())
	assert.Equal(t, false, syncInfo.SupportCORS)
	assert.Equal(t, "vrtcal", syncer.FamilyName())
}

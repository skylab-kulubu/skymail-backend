package tracking

import (
	"strings"
	"time"
)

// ClassifierVersion is stored on every event. A changed rule needs a new
// version; old events are never reinterpreted by a newer classifier.
const ClassifierVersion int16 = 1

type Classification struct {
	SourceClass     string
	AutomatedLikely bool
	Version         int16
}

type clientRule struct {
	class     string
	automatic bool
	matches   func(string) bool
}

// Ordered exactly as design note 07 §3.2. Raw UA lives only on the stack;
// neither it nor a request address is kept in an event.
var clientRules = []clientRule{
	{"apple_proxy_likely", true, func(ua string) bool { return ua == "mozilla/5.0" }},
	{"google_proxy", false, func(ua string) bool { return strings.Contains(ua, "googleimageproxy") }},
	{"yahoo_proxy", false, func(ua string) bool { return strings.Contains(ua, "yahoomailproxy") }},
	{"scanner_likely", true, func(ua string) bool {
		return ua == "" || containsAny(ua, "bot", "scan", "python", "curl", "go-http-client", "wget", "headless")
	}},
	{"microsoft", false, func(ua string) bool { return containsAny(ua, "outlook", "microsoft office", "microsoft-office") }},
	{"mail_client", false, func(ua string) bool {
		return containsAny(ua, "thunderbird", "applewebkit mail", "apple mail", "samsungemail", "mail/", "evolution")
	}},
	{"browser", false, func(ua string) bool {
		return containsAny(ua, "mozilla/", "chrome/", "safari/", "firefox/", "edg/", "opera/")
	}},
	{"scanner_likely", true, func(string) bool { return true }},
}

func containsAny(value string, parts ...string) bool {
	for _, part := range parts {
		if strings.Contains(value, part) {
			return true
		}
	}
	return false
}

func Classify(ua, method, kind string, sentAt, at time.Time, hasOpen bool) Classification {
	result := Classification{SourceClass: "unknown", Version: ClassifierVersion}
	for _, rule := range clientRules {
		if rule.matches(strings.ToLower(ua)) {
			result.SourceClass = rule.class
			result.AutomatedLikely = rule.automatic
			break
		}
	}
	if method == "HEAD" {
		result.SourceClass = "scanner_likely"
		result.AutomatedLikely = true
	}
	age := at.Sub(sentAt)
	if age <= 10*time.Second || ((result.SourceClass == "google_proxy" || result.SourceClass == "yahoo_proxy") && age <= 30*time.Second) || (kind == "click" && !hasOpen && age <= 60*time.Second) {
		result.AutomatedLikely = true
	}
	return result
}

package mailer_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/msauth"
)

const (
	laneTenant = "733f9063-0000-4000-8000-000000000001"
	laneClient = "90a70433-0000-4000-8000-000000000002"
)

// appKey is a throwaway key and self-signed certificate standing for the
// skymail-posta application's, made once.
var appKey = sync.OnceValues(func() (*rsa.PrivateKey, []byte) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "skymail-posta-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return key, der
})

// appEnv is GRAPH_* as Dokploy gives them: the key the one-line base64 of its
// PKCS#8 PEM (as the ops wizard stores it), the certificate the base64 of
// its .cer.
func appEnv() map[string]string {
	key, cert := appKey()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err)
	}
	return map[string]string{
		msauth.EnvTenantID:   laneTenant,
		msauth.EnvClientID:   laneClient,
		msauth.EnvClientKey:  base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		msauth.EnvClientCert: base64.StdEncoding.EncodeToString(cert),
	}
}

// decidedEnv is the decided configuration (hub ticket 10): hesap and bilgi
// through Graph, duyuru with XOAUTH2, Reply-To info@.
func decidedEnv() map[string]string {
	env := appEnv()
	env["SKYMAIL_SEND_HESAP"] = "graph"
	env["SKYMAIL_FROM_HESAP"] = "SKY LAB Hesap <hesap@bildirim.example.org>"
	env["SKYMAIL_SEND_BILGI"] = "graph"
	env["SKYMAIL_FROM_BILGI"] = "SKY LAB Bildirim Hizmeti <bilgi@bildirim.example.org>"
	env["SKYMAIL_SEND_DUYURU"] = "xoauth2"
	env["SKYMAIL_FROM_DUYURU"] = "SKY LAB Bildirim Hizmeti <duyuru@bildirim.example.org>"
	env["SMTP_REPLY_TO"] = "info@example.org"
	return env
}

func getenv(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

func ptr(s string) *string { return &s }

// A mail's lane: account mail (a System template keyed keycloak.* or
// account.*) is hesap in either queue lane; otherwise the bulk lane is duyuru
// and the system lane bilgi.
func TestLaneOfAMail(t *testing.T) {
	for _, tc := range []struct {
		name     string
		key      *string
		system   bool
		priority mailer.Priority
		want     mailer.Lane
	}{
		{"a login code", ptr("keycloak.login-code"), true, mailer.PrioritySystem, mailer.LaneHesap},
		{"an address verification", ptr("account.verify-email"), true, mailer.PrioritySystem, mailer.LaneHesap},
		{"account mail in the bulk lane", ptr("keycloak.reset-password"), true, mailer.PriorityBulk, mailer.LaneHesap},
		{"an operator's account.* template", ptr("account.welcome"), false, mailer.PrioritySystem, mailer.LaneBilgi},
		{"a Forms confirmation", ptr("forms.submission-received"), true, mailer.PrioritySystem, mailer.LaneBilgi},
		{"an event notification", ptr("event.reminder"), true, mailer.PrioritySystem, mailer.LaneBilgi},
		{"a single mail without a template", nil, false, mailer.PrioritySystem, mailer.LaneBilgi},
		{"a send to a list", ptr("duyuru-ekim"), false, mailer.PriorityBulk, mailer.LaneDuyuru},
		{"a bulk notification type", ptr("event.announcement"), true, mailer.PriorityBulk, mailer.LaneDuyuru},
		{"a row from before the lanes", nil, false, mailer.Priority(-1), mailer.LaneDuyuru},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mailer.LaneOf(tc.key, tc.system, tc.priority); got != tc.want {
				t.Errorf("lane %q, want %q", got, tc.want)
			}
		})
	}
}

// Unset, every lane is off and nothing else is read: production sends as it
// always has until a lane is switched on.
func TestSendLanesAreOffByDefault(t *testing.T) {
	lanes, err := mailer.SendLanesFromEnv(getenv(map[string]string{
		// Credentials alone switch nothing on, and are not even read.
		msauth.EnvClientKey: "not a key",
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, lane := range mailer.Lanes {
		if got := lanes.Lane(lane).Transport; got != mailer.TransportOff {
			t.Errorf("%s = %q", lane, got)
		}
	}
	if lanes.ReplyTo != nil || lanes.Credentials != nil {
		t.Errorf("reply-to %v, credentials %v", lanes.ReplyTo, lanes.Credentials)
	}
	if zero := (mailer.SendLanes{}).Lane(mailer.LaneHesap).Transport; zero != mailer.TransportOff {
		t.Errorf("the zero value's lane is %q", zero)
	}
}

func TestSendLanesFromEnv(t *testing.T) {
	lanes, err := mailer.SendLanesFromEnv(getenv(decidedEnv()))
	if err != nil {
		t.Fatal(err)
	}
	hesap := lanes.Lane(mailer.LaneHesap)
	if hesap.Transport != mailer.TransportGraph || hesap.From.Name != "SKY LAB Hesap" || hesap.From.Address != "hesap@bildirim.example.org" {
		t.Errorf("hesap = %+v %+v", hesap, hesap.From)
	}
	if duyuru := lanes.Lane(mailer.LaneDuyuru); duyuru.Transport != mailer.TransportXOAUTH2 {
		t.Errorf("duyuru = %+v", duyuru)
	}
	if lanes.ReplyTo == nil || lanes.ReplyTo.Address != "info@example.org" || lanes.Credentials == nil {
		t.Errorf("reply-to %v credentials %v", lanes.ReplyTo, lanes.Credentials)
	}

	// The Reply-To alone, every lane off: no credentials are needed.
	if lanes, err := mailer.SendLanesFromEnv(getenv(map[string]string{"SMTP_REPLY_TO": "SKY LAB <info@example.org>"})); err != nil || lanes.ReplyTo.Name != "SKY LAB" {
		t.Errorf("reply-to alone: %v %v", lanes.ReplyTo, err)
	}

	for name, tc := range map[string]struct {
		change map[string]string
		error  string
	}{
		"a typo":             {map[string]string{"SKYMAIL_SEND_BILGI": "grahp"}, "SKYMAIL_SEND_BILGI must be off, graph or xoauth2"},
		"no From":            {map[string]string{"SKYMAIL_FROM_HESAP": ""}, "SKYMAIL_SEND_HESAP=graph needs SKYMAIL_FROM_HESAP"},
		"a From that is not": {map[string]string{"SKYMAIL_FROM_DUYURU": "duyuru at example"}, "SKYMAIL_FROM_DUYURU is not an address"},
		"no certificate":     {map[string]string{msauth.EnvClientCert: ""}, "a sender lane is on: GRAPH_CLIENT_CERT is required"},
		"a bad Reply-To":     {map[string]string{"SMTP_REPLY_TO": "info"}, "SMTP_REPLY_TO is not an address"},
	} {
		t.Run(name, func(t *testing.T) {
			env := decidedEnv()
			for k, v := range tc.change {
				env[k] = v
			}
			_, err := mailer.SendLanesFromEnv(getenv(env))
			if err == nil || !strings.Contains(err.Error(), tc.error) {
				t.Fatalf("err = %v, want %q", err, tc.error)
			}
		})
	}

	// Upper case and spaces are read; OFF is off.
	env := decidedEnv()
	env["SKYMAIL_SEND_HESAP"] = " GRAPH "
	env["SKYMAIL_SEND_DUYURU"] = "OFF"
	lanes, err = mailer.SendLanesFromEnv(getenv(env))
	if err != nil || lanes.Lane(mailer.LaneHesap).Transport != mailer.TransportGraph || lanes.Lane(mailer.LaneDuyuru).Transport != mailer.TransportOff {
		t.Fatalf("%+v %v", lanes, err)
	}
}

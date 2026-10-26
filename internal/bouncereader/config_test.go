package bouncereader_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/skymail-backend/internal/bouncereader"
)

// credentialEnv is a throwaway application credential: a key and its
// certificate, as GRAPH_* hold them.
func credentialEnv(t *testing.T) map[string]string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "skymail-posta"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"GRAPH_TENANT_ID":   "kulup.onmicrosoft.com",
		"GRAPH_CLIENT_ID":   "90a70433-0000-4000-8000-000000000001",
		"GRAPH_CLIENT_KEY":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
		"GRAPH_CLIENT_CERT": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}
}

func getenv(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

// Unset or off, the reader reads nothing else from the environment: the
// sender lanes may be off and GRAPH_* unset.
func TestTheReaderIsOffByDefault(t *testing.T) {
	for _, value := range []string{"", "off", " off "} {
		config, err := bouncereader.ConfigFromEnv(getenv(map[string]string{"SKYMAIL_BOUNCE_READER": value}))
		if err != nil || config.Mode != bouncereader.ModeOff {
			t.Errorf("%q: config = %+v, err = %v", value, config, err)
		}
	}
}

// Any other value stops startup, as the other flags do.
func TestAnUnknownModeStopsStartup(t *testing.T) {
	for _, value := range []string{"true", "ON", "dryrun", "apply"} {
		if _, err := bouncereader.ConfigFromEnv(getenv(map[string]string{"SKYMAIL_BOUNCE_READER": value})); err == nil {
			t.Errorf("%q: no error", value)
		}
	}
}

// dry-run and on need the mailboxes and the application's credentials; the
// mailboxes are bare addresses, lower-cased, each once.
func TestDryRunAndOnNeedTheMailboxesAndTheCredentials(t *testing.T) {
	env := credentialEnv(t)
	env["SKYMAIL_BOUNCE_READER"] = "dry-run"
	if _, err := bouncereader.ConfigFromEnv(getenv(env)); err == nil || !strings.Contains(err.Error(), "SKYMAIL_BOUNCE_MAILBOXES") {
		t.Errorf("without mailboxes: err = %v", err)
	}
	env["SKYMAIL_BOUNCE_MAILBOXES"] = " Hesap@Bildirim.Example.org, bilgi@bildirim.example.org,hesap@bildirim.example.org ,"
	config, err := bouncereader.ConfigFromEnv(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != bouncereader.ModeDryRun || strings.Join(config.Mailboxes, " ") != "hesap@bildirim.example.org bilgi@bildirim.example.org" || config.Credentials == nil {
		t.Errorf("config = %+v", config)
	}

	for _, bad := range []string{"SKY LAB <hesap@bildirim.example.org>", "hesap", "hesap@bildirim.example.org;bilgi@bildirim.example.org"} {
		env["SKYMAIL_BOUNCE_MAILBOXES"] = bad
		if _, err := bouncereader.ConfigFromEnv(getenv(env)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}

	env["SKYMAIL_BOUNCE_MAILBOXES"] = "hesap@bildirim.example.org"
	env["SKYMAIL_BOUNCE_READER"] = "on"
	delete(env, "GRAPH_CLIENT_CERT")
	_, err = bouncereader.ConfigFromEnv(getenv(env))
	if err == nil || !strings.Contains(err.Error(), "GRAPH_CLIENT_CERT") {
		t.Errorf("without the certificate: err = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Errorf("the error quotes the key: %v", err)
	}
}

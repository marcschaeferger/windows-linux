package logging

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fosrl/newt/logger"
)

func TestWriterRedactsAndKeepsFormat(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.loc = time.UTC
	ts := time.Date(2026, 11, 14, 8, 30, 5, 0, time.UTC)

	w.Write(logger.DEBUG, ts, "Build tunnel called: config: {Secret:topsecretvalue UserToken:tokentokentoken}")
	want := "DEBUG: 2026/11/14 08:30:05 Build tunnel called: config: {Secret:[REDACTED] UserToken:[REDACTED]}\n"
	if buf.String() != want {
		t.Errorf("got %q want %q", buf.String(), want)
	}
	if strings.Contains(buf.String(), "topsecretvalue") {
		t.Error("secret leaked")
	}
}

// TestInstallRedactsEverythingTheSharedLoggerWrites covers the lines other
// libraries log through the newt logger, such as olm's dump of its config.
// It must be the only test that installs the process-wide logger.
func TestInstallRedactsEverythingTheSharedLoggerWrites(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenRotatingFile(dir+"/pangolin.log", FileOptions{Owner: true})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := NewWriter(f)
	if !Install(w) {
		t.Fatal("the shared logger was already initialized")
	}
	logger.GetLogger().SetLevel(logger.DEBUG)

	logger.Debug("Starting tunnel with config: %+v", struct{ Secret, UserToken string }{"olmsecret-1234567", "token-value-7654321"})
	logger.Error(`Requesting token ... with body: {"olmId":"x","secret":"hush-hush-1234"}`)

	data, err := os.ReadFile(dir + "/pangolin.log")
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, leak := range []string{"olmsecret-1234567", "token-value-7654321", "hush-hush-1234"} {
		if strings.Contains(out, leak) {
			t.Errorf("log file leaks %q:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "DEBUG: ") || !strings.Contains(out, "ERROR: ") || strings.Count(out, "\n") != 2 {
		t.Errorf("unexpected log format:\n%s", out)
	}
}

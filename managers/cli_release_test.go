package managers

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sumOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestParseAssetDigest(t *testing.T) {
	good := sumOf([]byte("x"))
	for _, tt := range []struct {
		digest  string
		want    string
		wantErr bool
	}{
		{"sha256:" + good, good, false},
		{"sha256:" + strings.ToUpper(good), good, false},
		{"", "", true},                                  // older releases publish none: fail closed
		{"sha1:abcd", "", true},                         // wrong algorithm
		{"sha256:abcd", "", true},                       // too short
		{"sha256:" + strings.Repeat("z", 64), "", true}, // not hex
	} {
		got, err := parseAssetDigest(tt.digest)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("parseAssetDigest(%q) = %q, %v", tt.digest, got, err)
		}
	}
}

func TestAssetURLRequiresDigest(t *testing.T) {
	r := &githubReleaseResponse{Assets: []githubReleaseAsset{
		{Name: "with", URL: "https://example/with", Digest: "sha256:" + sumOf([]byte("a"))},
		{Name: "without", URL: "https://example/without"},
	}}
	if url, sum, err := r.assetURL("with"); err != nil || url != "https://example/with" || sum != sumOf([]byte("a")) {
		t.Fatalf("assetURL(with) = %q, %q, %v", url, sum, err)
	}
	if _, _, err := r.assetURL("without"); err == nil {
		t.Fatal("assetURL accepted an asset without a digest")
	}
	if _, _, err := r.assetURL("missing"); err == nil {
		t.Fatal("assetURL found a missing asset")
	}
}

func TestDownloadCLIAssetVerifiesSHA256(t *testing.T) {
	payload := []byte("pretend this is a CLI binary")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(payload) }))
	defer srv.Close()
	dir := t.TempDir()

	ok := filepath.Join(dir, "ok")
	if err := downloadCLIAsset(srv.URL, ok, sumOf(payload)); err != nil {
		t.Fatalf("download with the right digest: %v", err)
	}
	if got, _ := os.ReadFile(ok); string(got) != string(payload) {
		t.Fatalf("file content = %q", got)
	}

	bad := filepath.Join(dir, "bad")
	err := downloadCLIAsset(srv.URL, bad, sumOf([]byte("something else")))
	if err == nil || !strings.Contains(err.Error(), "wrong sha256") {
		t.Fatalf("download with the wrong digest: err = %v", err)
	}
	if _, statErr := os.Stat(bad); !os.IsNotExist(statErr) {
		t.Fatal("a download that failed verification was left on disk")
	}
}

package nineinbox

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAssetsIncludeHonestAccessibleInboxInterface(t *testing.T) {
	for name, required := range map[string][]string{
		"index.html": {
			"Send yourself anything", "No account. No 09C.", "Create my inbox", "Pair this device",
			"Encrypted on this device", "Syncs while this page is open", "Files expire after 7 days", "20 MiB",
			`id="setup-view"`, `id="pair-view"`, `id="inbox-view"`, `id="composer"`, `id="inbox-list"`,
			`id="pairing-qr"`, `id="pairing-words"`, `id="storage-meter"`, `id="recovery-export"`,
			`id="recovery-file"`, `id="restore-recovery"`, `id="delete-inbox"`,
			`aria-live="polite"`, `aria-label="Search your inbox"`, `type="module"`,
			`href="/"`, `href="/privacy.html"`, `href="/terms.html"`, `class="service-links"`,
		},
		"app.css": {
			"--signal: #ff5a1f", ".inbox-stream", ".composer-sheet", ".pairing-grid",
			"min-height: 44px", "@media (max-width: 640px)", "@media (prefers-reduced-motion: reduce)",
			`font-family: "Segoe UI"`, ".service-links",
		},
		"app.mjs": {
			`from "./crypto.mjs"`, `from "./storage.mjs"`, `from "./qr.mjs"`, "/api/nine/v1/inboxes",
			"createItemId", "encryptItem", "decryptItem", "visibilitychange", "serviceWorker.register",
			`updateViaCache: "none"`,
			"importRecoveryFile", "restoreRecovery", "deleteSharedInbox", "scheduleSync", "setTimeout(scheduleSync, 15000)",
		},
	} {
		body, err := WebFS.ReadFile("web/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		content := string(body)
		for _, value := range required {
			if !strings.Contains(content, value) {
				t.Errorf("%s is missing %q", name, value)
			}
		}
	}

	css, err := WebFS.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		`font-size: clamp(34px, 5vw, 62px)`,
		`.setup-copy::before`,
		`font: 230px/.8`,
	} {
		if strings.Contains(string(css), forbidden) {
			t.Errorf("app.css still contains oversized/decorative treatment %q", forbidden)
		}
	}

	for _, name := range []string{"index.html", "app.css", "app.mjs"} {
		body, err := WebFS.ReadFile("web/" + name)
		if err != nil {
			t.Fatal(err)
		}
		content := string(body)
		for _, forbidden := range []string{"fonts.googleapis", "unpkg.com", "cdn.jsdelivr", "analytics", "navigator.clipboard.read", "guaranteed background", "virus scanned"} {
			if strings.Contains(strings.ToLower(content), strings.ToLower(forbidden)) {
				t.Errorf("%s contains forbidden external or misleading token %q", name, forbidden)
			}
		}
	}
}

func TestManifestIsInstallableAndDeclaresManualShareTarget(t *testing.T) {
	body, err := WebFS.ReadFile("web/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Name        string `json:"name"`
		StartURL    string `json:"start_url"`
		Scope       string `json:"scope"`
		Display     string `json:"display"`
		ShareTarget struct {
			Action  string `json:"action"`
			Method  string `json:"method"`
			Enctype string `json:"enctype"`
			Params  struct {
				Text  string `json:"text"`
				URL   string `json:"url"`
				Files []struct {
					Name   string   `json:"name"`
					Accept []string `json:"accept"`
				} `json:"files"`
			} `json:"params"`
		} `json:"share_target"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "Nine Inbox" || manifest.StartURL != "/inbox/" || manifest.Scope != "/inbox/" || manifest.Display != "standalone" {
		t.Fatalf("manifest identity = %#v", manifest)
	}
	if manifest.ShareTarget.Action != "/inbox/share" || manifest.ShareTarget.Method != "POST" || manifest.ShareTarget.Enctype != "multipart/form-data" ||
		manifest.ShareTarget.Params.Text == "" || manifest.ShareTarget.Params.URL == "" || len(manifest.ShareTarget.Params.Files) != 1 {
		t.Fatalf("share target = %#v", manifest.ShareTarget)
	}
}

func TestSiteHandlerServesOnlyEmbeddedInboxAssetsWithSecurityHeaders(t *testing.T) {
	server := httptest.NewServer(NewSiteHandler(http.NotFoundHandler()))
	defer server.Close()

	for _, path := range []string{
		"/inbox/", "/inbox/app.css", "/inbox/app.mjs", "/inbox/crypto.mjs", "/inbox/storage.mjs",
		"/inbox/qr.mjs", "/inbox/manifest.webmanifest", "/inbox/service-worker.js", "/inbox/icon.svg",
	} {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d", path, response.StatusCode)
		}
		for name, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := response.Header.Get(name); got != want {
				t.Errorf("GET %s %s = %q, want %q", path, name, got, want)
			}
		}
		if !strings.Contains(response.Header.Get("Content-Security-Policy"), "default-src 'self'") ||
			!strings.Contains(response.Header.Get("Content-Security-Policy"), "connect-src 'self'") {
			t.Errorf("GET %s CSP = %q", path, response.Header.Get("Content-Security-Policy"))
		}
		wantCache := "public, max-age=3600"
		if path == "/inbox/" || path == "/inbox/manifest.webmanifest" || path == "/inbox/service-worker.js" {
			wantCache = "no-store"
		}
		if got := response.Header.Get("Cache-Control"); got != wantCache {
			t.Errorf("GET %s Cache-Control = %q, want %q", path, got, wantCache)
		}
	}

	response, err := http.Get(server.URL + "/inbox/not-a-file")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown asset status = %d", response.StatusCode)
	}
}

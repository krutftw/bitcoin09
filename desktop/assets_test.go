package desktop

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedInterfaceIsOfflineAndComplete(t *testing.T) {
	files := []string{"assets/index.html", "assets/app.css", "assets/app.js", "assets/icon.svg"}
	contents := make(map[string]string, len(files))
	for _, name := range files {
		body, err := fs.ReadFile(assetsFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		contents[name] = string(body)
		lower := strings.ToLower(contents[name])
		if name == "assets/index.html" {
			lower = strings.Replace(lower, `href="https://btc09.org/inbox/"`, "", 1)
			lower = strings.Replace(lower, `href="https://btc09.org/#mining-guide"`, "", 1)
		}
		if strings.Contains(lower, "https://") || strings.Contains(lower, "http://") || strings.Contains(lower, "//cdn") {
			t.Fatalf("%s contains an external resource", name)
		}
	}

	html := contents["assets/index.html"]
	for _, required := range []string{
		`<main`, `aria-live="polite"`, `id="first-run"`, `id="wallet-view"`,
		`rel="icon"`, `href="/assets/icon.svg"`,
		`id="create-wallet"`, `id="receive-address"`, `id="copy-address"`,
		`id="new-address"`, `id="backup-wallet"`, `id="send-form"`,
		`id="review-payment"`, `id="confirm-send"`, `id="send-result"`,
		`class="wallet-frame"`, `class="account-summary"`, `class="quick-actions"`,
	} {
		if !strings.Contains(html, required) {
			t.Errorf("index is missing %q", required)
		}
	}
	if strings.Contains(html, "style=") || strings.Contains(html, "onclick=") {
		t.Fatal("index uses inline presentation or event handlers")
	}
	if strings.Contains(html, "Your 09C,<") {
		t.Fatal("interface still contains the oversized marketing masthead")
	}

	javascript := contents["assets/app.js"]
	for _, required := range []string{
		`/api/v1/status`, `/api/v1/wallet/create`, `/api/v1/wallet/address`,
		`/api/v1/wallet/backup`, `/api/v1/send/preview`, `/api/v1/send/confirm`,
		`X-BTC09-CSRF`, `navigator.clipboard`, `pending_id`, `formatCoins`, `address-chip-text`,
	} {
		if !strings.Contains(javascript, required) {
			t.Errorf("app script is missing %q", required)
		}
	}

	css := contents["assets/app.css"]
	for _, required := range []string{
		`--ink:`, `--copper:`, `@media (max-width:`, `prefers-reduced-motion: reduce`,
		`:focus-visible`, `.signal-field`, `.ledger`, `.review-sheet`,
		`--text-xs: 12px;`, `--text-sm: 14px;`, `--display-max: 76px;`,
	} {
		if !strings.Contains(css, required) {
			t.Errorf("app stylesheet is missing %q", required)
		}
	}
	for _, generic := range []string{"Arial", "Inter", "Roboto", "purple"} {
		if strings.Contains(css, generic) {
			t.Errorf("app stylesheet contains generic design token %q", generic)
		}
	}
}

func TestEmbeddedInterfaceExplainsFastAndFullWalletModes(t *testing.T) {
	for _, test := range []struct {
		name     string
		required []string
	}{
		{name: "assets/index.html", required: []string{`id="wallet-mode"`, `WALLET CONNECTION`, `Keys stay on this computer`}},
		{name: "assets/app.js", required: []string{`status.mode`, `status.balance_available`, `FAST MODE`, `FULL NODE`, `Wallet service`}},
		{name: "assets/app.css", required: []string{`.mode-value`, `.status-lamp.is-ready`}},
	} {
		body, err := fs.ReadFile(assetsFS, test.name)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range test.required {
			if !strings.Contains(string(body), required) {
				t.Errorf("%s is missing %q", test.name, required)
			}
		}
	}
}

func TestEmbeddedInterfaceIncludesHonestOfficialSoloMiner(t *testing.T) {
	tests := []struct {
		name      string
		required  []string
		forbidden []string
	}{
		{
			name: "assets/index.html",
			required: []string{
				`data-panel="miner-panel"`, `id="miner-panel"`, `Open solo`,
				`id="miner-workers"`, `id="miner-worker"`, `id="start-miner"`, `id="stop-miner"`,
				`id="miner-current-hashrate"`, `id="miner-average-hashrate"`,
				`id="miner-total-hashes"`, `id="miner-blocks"`, `id="miner-state"`,
				`No partial-share payouts`, `Only your public payout address`,
			},
			forbidden: []string{`guaranteed profit`, `GPU mining`, `pool balance`},
		},
		{
			name: "assets/app.js",
			required: []string{
				`/api/v1/miner/status`, `/api/v1/miner/start`, `/api/v1/miner/stop`,
				`logical_cpus`, `current_hashrate`, `average_hashrate`, `blocks_accepted`,
				`setTimeout(refreshMinerStatus, 1000)`, `minerPollTimer`,
			},
		},
		{
			name: "assets/app.css",
			required: []string{
				`.miner-instruments`, `.miner-control-grid`, `.miner-state-line`, `.miner-actions`,
				`grid-template-columns: repeat(2, minmax(0, 1fr))`,
			},
		},
	}
	for _, test := range tests {
		body, err := fs.ReadFile(assetsFS, test.name)
		if err != nil {
			t.Fatal(err)
		}
		content := string(body)
		for _, required := range test.required {
			if !strings.Contains(content, required) {
				t.Errorf("%s is missing %q", test.name, required)
			}
		}
		for _, forbidden := range test.forbidden {
			if strings.Contains(strings.ToLower(content), strings.ToLower(forbidden)) {
				t.Errorf("%s contains forbidden claim %q", test.name, forbidden)
			}
		}
	}
}

func TestEmbeddedMinerSupportDiagnosticsAreUsefulAndPrivate(t *testing.T) {
	htmlBody, err := fs.ReadFile(assetsFS, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBody)
	for _, required := range []string{
		`class="miner-readiness"`, `id="miner-wallet-check"`, `id="miner-endpoint-check"`,
		`id="miner-cpu-check"`, `id="miner-cpu-guidance"`, `id="copy-miner-report"`,
		`href="https://btc09.org/#mining-guide"`, `Leave one thread free`, `Copy help report`,
		`id="copy-miner-report" class="quiet-action miner-copy-action"`,
	} {
		if !strings.Contains(html, required) {
			t.Errorf("index is missing %q", required)
		}
	}

	javascriptBody, err := fs.ReadFile(assetsFS, "assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	javascript := string(javascriptBody)
	for _, required := range []string{
		`function minerSupportReport(status)`, `BTC09 miner help report`, `Version:`, `Wallet mode:`,
		`Miner state:`, `CPU threads:`, `Jobs:`, `Reconnects:`, `Last error:`,
		`status.jobs > 0 ? "Last check passed" : "Not tested"`,
		`navigator.clipboard.writeText(minerSupportReport(state.miner))`,
	} {
		if !strings.Contains(javascript, required) {
			t.Errorf("app script is missing %q", required)
		}
	}
	start := strings.Index(javascript, "function minerSupportReport(status)")
	if start < 0 {
		t.Fatal("could not find miner support report function")
	}
	end := strings.Index(javascript[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not isolate miner support report function")
	}
	reportFunction := javascript[start : start+end]
	for _, private := range []string{"status.address", "status.worker", "wallet_path", "addresses"} {
		if strings.Contains(reportFunction, private) {
			t.Errorf("support report includes private field %q", private)
		}
	}

	cssBody, err := fs.ReadFile(assetsFS, "assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`.miner-readiness`, `.miner-help-row`, `.miner-cpu-guidance`} {
		if !strings.Contains(string(cssBody), required) {
			t.Errorf("app stylesheet is missing %q", required)
		}
	}
}

func TestEmbeddedInterfaceLinksToNineInboxWithoutChangingWalletActions(t *testing.T) {
	htmlBody, err := fs.ReadFile(assetsFS, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBody)
	for _, required := range []string{
		`class="inbox-utility"`, `href="https://btc09.org/inbox/"`,
		`target="_blank"`, `rel="noopener noreferrer"`, `Nine Inbox`,
		`No account or 09C needed`,
	} {
		if !strings.Contains(html, required) {
			t.Errorf("index is missing %q", required)
		}
	}
	if count := strings.Count(html, `data-panel=`); count != 4 {
		t.Fatalf("wallet action count = %d, want 4", count)
	}
	cssBody, err := fs.ReadFile(assetsFS, "assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`.inbox-utility`, `.inbox-utility strong`, `.inbox-utility small`} {
		if !strings.Contains(string(cssBody), required) {
			t.Errorf("app stylesheet is missing %q", required)
		}
	}
}

func TestAuthenticatedRootServesInterfaceAndAssets(t *testing.T) {
	server := testServer(t)
	cookie, _ := launchSession(t, server)
	for _, test := range []struct {
		path        string
		contentType string
		contains    string
	}{
		{path: "/", contentType: "text/html", contains: "BTC09 Wallet"},
		{path: "/assets/app.css", contentType: "text/css", contains: "--copper"},
		{path: "/assets/app.js", contentType: "text/javascript", contains: "/api/v1/status"},
		{path: "/assets/icon.svg", contentType: "image/svg+xml", contains: "<svg"},
	} {
		req := httptest.NewRequest(http.MethodGet, test.path, nil)
		req.Host = "127.0.0.1:49152"
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		server.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Header().Get("Content-Type"), test.contentType) || !strings.Contains(rr.Body.String(), test.contains) {
			t.Errorf("GET %s: status=%d type=%q body=%q", test.path, rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
		}
	}

	unauthenticated := httptest.NewRequest(http.MethodGet, "/", nil)
	unauthenticated.Host = "127.0.0.1:49152"
	unauthenticatedRR := httptest.NewRecorder()
	server.ServeHTTP(unauthenticatedRR, unauthenticated)
	if unauthenticatedRR.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated root status = %d", unauthenticatedRR.Code)
	}
}

func TestReceiveQRIsAuthenticatedBoundedPNG(t *testing.T) {
	server := testServer(t)
	cookie, _ := launchSession(t, server)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/receive-qr?address=09TestAddress123", nil)
	req.Host = "127.0.0.1:49152"
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "image/png" || rr.Body.Len() < 100 || rr.Body.Len() > 100000 {
		t.Fatalf("QR response status=%d type=%q bytes=%d body=%q", rr.Code, rr.Header().Get("Content-Type"), rr.Body.Len(), rr.Body.String())
	}

	tooLong := httptest.NewRequest(http.MethodGet, "/api/v1/receive-qr?address="+strings.Repeat("a", 300), nil)
	tooLong.Host = "127.0.0.1:49152"
	tooLong.AddCookie(cookie)
	tooLongRR := httptest.NewRecorder()
	server.ServeHTTP(tooLongRR, tooLong)
	if tooLongRR.Code != http.StatusBadRequest {
		t.Fatalf("long QR status = %d", tooLongRR.Code)
	}
}

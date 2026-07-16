package desktop

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedInterfaceIsOfflineAndComplete(t *testing.T) {
	files := []string{"assets/index.html", "assets/app.css", "assets/network.js", "assets/app.js", "assets/icon.svg"}
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
		if name == "assets/app.js" {
			// Explorer links are user actions, not resources loaded by the interface.
			lower = strings.Replace(lower, `https://explorer.btc09.org/tx/`, "", 1)
		}
		if name == "assets/icon.svg" {
			lower = strings.Replace(lower, `xmlns="http://www.w3.org/2000/svg"`, "", 1)
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
		`/api/v1/status`, `/api/v1/wallet/v2/create`, `/api/v1/wallet/address`,
		`/api/v1/wallet/backup`, `/api/v1/send/preview`, `/api/v1/send/confirm`,
		`BTC09Network.request`, `navigator.clipboard`, `pending_id`, `formatCoins`, `address-chip-text`,
	} {
		if !strings.Contains(javascript, required) {
			t.Errorf("app script is missing %q", required)
		}
	}
	network := contents["assets/network.js"]
	for _, required := range []string{`X-BTC09-CSRF`, `credentials: "same-origin"`, `BTC09 Wallet lost contact with the app`} {
		if !strings.Contains(network, required) {
			t.Errorf("network helper is missing %q", required)
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

func TestFirstRunKeepsThePrimaryActionInACommonLaptopViewport(t *testing.T) {
	htmlBody, err := fs.ReadFile(assetsFS, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBody)
	for _, required := range []string{
		`<details class="file-location">`,
		`<summary>Stored on this device</summary>`,
		`<code id="first-run-path">`,
	} {
		if !strings.Contains(html, required) {
			t.Errorf("friendly wallet location is missing %q", required)
		}
	}

	cssBody, err := fs.ReadFile(assetsFS, "assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBody)
	for _, required := range []string{
		`main { min-width: 0; padding: 18px 28px 28px;`,
		`.first-run { width: min(540px, 100%); margin: 6px auto 12px; padding: 24px 32px;`,
		`width: 48px; height: 48px; margin-bottom: 14px;`,
		`.file-location { margin: 14px 0 12px; padding: 12px;`,
		`.setup-switch { margin: 12px 0;`,
		`.full-action { width: 100%; margin-top: 16px;`,
		`.file-location summary::after { content: "Show location";`,
		`.file-location[open] summary::after { content: "Hide location";`,
		`.setup-mark { display: none; }`,
	} {
		if !strings.Contains(css, required) {
			t.Errorf("compact first-run layout is missing %q", required)
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

func TestEmbeddedInterfaceSupportsEncryptedRecoveryWalletLifecycle(t *testing.T) {
	htmlBody, err := fs.ReadFile(assetsFS, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBody)
	for _, required := range []string{
		`id="create-wallet-form"`, `id="create-password"`, `id="create-password-confirm"`,
		`id="show-restore"`, `id="restore-wallet-form"`, `id="restore-phrase"`,
		`id="locked-view"`, `id="unlock-wallet-form"`, `id="unlock-password"`,
		`id="recovery-backup"`, `id="recovery-word-grid"`, `id="confirm-recovery-backup"`,
		`24 recovery words`, `at least 12 characters`,
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
		`/api/v1/wallet/v2/create`, `/api/v1/wallet/v2/restore`, `/api/v1/wallet/v2/unlock`, `/api/v1/wallet/v2/recovery`,
		`status.needs_unlock`, `status.wallet_version`, `recovery_phrase`, `state.recoveryPhrase = null`,
	} {
		if !strings.Contains(javascript, required) {
			t.Errorf("app script is missing %q", required)
		}
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage", "indexedDB"} {
		if strings.Contains(javascript, forbidden) {
			t.Errorf("app script persists recovery material through %s", forbidden)
		}
	}

	cssBody, err := fs.ReadFile(assetsFS, "assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`.setup-switch`, `.password-stack`, `.recovery-word-grid`, `.locked-view`} {
		if !strings.Contains(string(cssBody), required) {
			t.Errorf("app stylesheet is missing %q", required)
		}
	}
}

func TestEmbeddedInterfaceIncludesVerifiedPPLNSMiner(t *testing.T) {
	tests := []struct {
		name      string
		required  []string
		forbidden []string
	}{
		{
			name: "assets/index.html",
			required: []string{
				`data-panel="miner-panel"`, `id="miner-panel"`, `PPLNS pool`,
				`id="miner-workers"`, `id="miner-worker"`, `id="start-miner"`, `id="stop-miner"`,
				`id="miner-current-hashrate"`, `id="miner-average-hashrate"`,
				`id="miner-shares"`, `id="miner-blocks"`, `id="miner-state"`,
				`Direct payouts`, `0% pool fee`, `Only your public payout address`,
			},
			forbidden: []string{`guaranteed profit`, `GPU mining`, `pool balance`, `Open solo`},
		},
		{
			name: "assets/app.js",
			required: []string{
				`/api/v1/miner/status`, `/api/v1/miner/start`, `/api/v1/miner/stop`,
				`logical_cpus`, `current_hashrate`, `average_hashrate`, `shares_accepted`, `blocks_accepted`, `pool_fee_bps`,
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
	javascript := strings.ReplaceAll(string(javascriptBody), "\r\n", "\n")
	for _, required := range []string{
		`function minerSupportReport(status)`, `BTC09 miner help report`, `Version:`, `Wallet mode:`,
		`Miner state:`, `Pool mode:`, `Pool fee:`, `Shares:`, `Blocks:`, `CPU threads:`, `Jobs:`, `Reconnects:`, `Last error:`,
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
	if count := strings.Count(html, `data-panel=`); count != 5 {
		t.Fatalf("wallet action count = %d, want 5", count)
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

func TestEmbeddedInterfaceIncludesClearWalletActivityMaxAndCleanup(t *testing.T) {
	htmlBody, err := fs.ReadFile(assetsFS, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBody)
	for _, required := range []string{
		`READY TO SEND`, `id="mining-rewards"`,
		`data-panel="activity-panel"`, `id="activity-panel"`, `id="activity-list"`,
		`id="send-max"`, `id="review-input-count"`,
		`id="cleanup-card"`, `id="preview-cleanup"`, `id="review-cleanup"`, `id="confirm-cleanup"`,
		`Received`, `Sent`, `Mining reward`, `Wallet cleanup`,
		`id="copy-result-txid"`, `id="open-result-txid"`,
	} {
		if !strings.Contains(html, required) {
			t.Errorf("index is missing %q", required)
		}
	}
	for _, forbidden := range []string{"PRIVATE KEY", "SEED PHRASE", "SIGNED TRANSACTION", "RAW SCRIPT"} {
		if strings.Contains(strings.ToUpper(html), forbidden) {
			t.Errorf("index exposes technical secret label %q", forbidden)
		}
	}

	javascriptBody, err := fs.ReadFile(assetsFS, "assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	javascript := string(javascriptBody)
	for _, required := range []string{
		`/api/v1/activity`, `/api/v1/send/max-preview`,
		`/api/v1/maintenance/cleanup/preview`, `/api/v1/maintenance/cleanup/confirm`,
		`/api/v1/preview/cancel`, `cancelPendingPreview`,
		`setTimeout(refreshActivity, 30000)`, `cleanup_recommended`, `selected_inputs.length`,
		`https://explorer.btc09.org/tx/`,
	} {
		if !strings.Contains(javascript, required) {
			t.Errorf("app script is missing %q", required)
		}
	}

	cssBody, err := fs.ReadFile(assetsFS, "assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBody)
	for _, required := range []string{
		`.activity-list`, `.activity-row`, `.cleanup-card`, `.amount-with-max`, `.result-actions`,
		`grid-template-columns: repeat(5, minmax(0, 1fr))`,
	} {
		if !strings.Contains(css, required) {
			t.Errorf("app stylesheet is missing %q", required)
		}
	}
}

func TestWalletOnlyBuildRemovesMiningFromNavigation(t *testing.T) {
	javascriptBody, err := fs.ReadFile(assetsFS, "assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	javascript := string(javascriptBody)
	for _, required := range []string{
		`function setMinerAvailable(available)`,
		`minerTab.hidden = !available`,
		`status.mining_available !== false`,
		`error.code === "miner_unavailable"`,
	} {
		if !strings.Contains(javascript, required) {
			t.Errorf("wallet-only navigation is missing %q", required)
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
		{path: "/assets/network.js", contentType: "text/javascript", contains: "BTC09Network"},
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

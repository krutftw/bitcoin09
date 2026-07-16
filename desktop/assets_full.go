//go:build !walletedition

package desktop

import (
	"bytes"
	"embed"
	"errors"
)

//go:embed assets/miner.css assets/miner.js assets/miner-head.html assets/miner-tab.html assets/miner-panel.html assets/miner-script.html
var editionAssets embed.FS

func editionAssetRoute(path string) (assetRoute, bool) {
	switch path {
	case "/assets/miner.css":
		return assetRoute{name: "assets/miner.css", contentType: "text/css; charset=utf-8"}, true
	case "/assets/miner.js":
		return assetRoute{name: "assets/miner.js", contentType: "text/javascript; charset=utf-8"}, true
	default:
		return assetRoute{}, false
	}
}

func readEditionAsset(name string) ([]byte, bool, error) {
	if name != "assets/miner.css" && name != "assets/miner.js" {
		return nil, false, nil
	}
	body, err := editionAssets.ReadFile(name)
	return body, true, err
}

func decorateEditionIndex(index []byte) ([]byte, error) {
	replacements := []struct {
		marker string
		asset  string
	}{
		{marker: "<!-- edition-head -->", asset: "assets/miner-head.html"},
		{marker: "<!-- edition-tab -->", asset: "assets/miner-tab.html"},
		{marker: "<!-- edition-panel -->", asset: "assets/miner-panel.html"},
		{marker: "<!-- edition-script -->", asset: "assets/miner-script.html"},
	}
	result := append([]byte(nil), index...)
	for _, replacement := range replacements {
		marker := []byte(replacement.marker)
		if !bytes.Contains(result, marker) {
			return nil, errors.New("desktop edition asset marker is missing")
		}
		fragment, err := editionAssets.ReadFile(replacement.asset)
		if err != nil {
			return nil, err
		}
		result = bytes.Replace(result, marker, bytes.TrimSpace(fragment), 1)
	}
	return result, nil
}

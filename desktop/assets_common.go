package desktop

import "embed"

//go:embed assets/index.html assets/app.css assets/network.js assets/app.js assets/icon.svg
var commonAssets embed.FS

type assetRoute struct {
	name        string
	contentType string
}

func commonAssetRoute(path string) (assetRoute, bool) {
	switch path {
	case "/":
		return assetRoute{name: "assets/index.html", contentType: "text/html; charset=utf-8"}, true
	case "/assets/app.css":
		return assetRoute{name: "assets/app.css", contentType: "text/css; charset=utf-8"}, true
	case "/assets/network.js":
		return assetRoute{name: "assets/network.js", contentType: "text/javascript; charset=utf-8"}, true
	case "/assets/app.js":
		return assetRoute{name: "assets/app.js", contentType: "text/javascript; charset=utf-8"}, true
	case "/assets/icon.svg":
		return assetRoute{name: "assets/icon.svg", contentType: "image/svg+xml; charset=utf-8"}, true
	default:
		return assetRoute{}, false
	}
}

func assetRouteForPath(path string) (string, string, bool) {
	if route, ok := commonAssetRoute(path); ok {
		return route.name, route.contentType, true
	}
	if route, ok := editionAssetRoute(path); ok {
		return route.name, route.contentType, true
	}
	return "", "", false
}

func isAssetPath(path string) bool {
	_, _, ok := assetRouteForPath(path)
	return ok
}

func readAsset(name string) ([]byte, error) {
	if body, ok, err := readEditionAsset(name); ok || err != nil {
		return body, err
	}
	body, err := commonAssets.ReadFile(name)
	if err != nil {
		return nil, err
	}
	if name == "assets/index.html" {
		return decorateEditionIndex(body)
	}
	return body, nil
}

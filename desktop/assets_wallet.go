//go:build walletedition

package desktop

func editionAssetRoute(string) (assetRoute, bool) { return assetRoute{}, false }

func readEditionAsset(string) ([]byte, bool, error) { return nil, false, nil }

func decorateEditionIndex(index []byte) ([]byte, error) { return index, nil }

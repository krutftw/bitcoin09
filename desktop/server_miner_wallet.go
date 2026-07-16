//go:build walletedition

package desktop

import "net/http"

func (*Server) handleEditionRoute(http.ResponseWriter, *http.Request) bool { return false }

//go:build !walletedition

package desktop

import "net/http"

func (s *Server) handleEditionRoute(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/api/v1/miner/status":
		s.handleMinerStatus(w, r)
	case "/api/v1/miner/start":
		s.handleMinerStart(w, r)
	case "/api/v1/miner/stop":
		s.handleMinerStop(w, r)
	default:
		return false
	}
	return true
}

func (s *Server) minerService(w http.ResponseWriter) (MinerService, bool) {
	service, ok := s.service.(MinerService)
	if !ok {
		s.writeError(w, http.StatusNotImplemented, "miner_unavailable", "This BTC09 build does not include the official miner.")
		return nil, false
	}
	return service, true
}

func (s *Server) handleMinerStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeRead(w, r); !ok {
		return
	}
	service, ok := s.minerService(w)
	if !ok {
		return
	}
	status, err := service.MinerStatus(r.Context())
	if err != nil {
		s.writeServiceError(w, err, "miner_status_failed", "BTC09 could not read the miner status.")
		return
	}
	s.writeData(w, http.StatusOK, status)
}

func (s *Server) handleMinerStart(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeMutation(w, r); !ok {
		return
	}
	var request MinerStartRequest
	if err := decodeJSONRequest(w, r, &request); err != nil {
		s.writeError(w, http.StatusBadRequest, "miner_start_invalid", "Choose a valid worker name and CPU thread count.")
		return
	}
	service, ok := s.minerService(w)
	if !ok {
		return
	}
	status, err := service.StartMiner(r.Context(), request)
	if err != nil {
		s.writeServiceError(w, err, "miner_start_failed", "BTC09 could not start the miner.")
		return
	}
	s.writeData(w, http.StatusOK, status)
}

func (s *Server) handleMinerStop(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeMutation(w, r); !ok {
		return
	}
	if err := decodeJSONRequest(w, r, &struct{}{}); err != nil {
		s.writeError(w, http.StatusBadRequest, "miner_stop_invalid", "The stop request was not valid.")
		return
	}
	service, ok := s.minerService(w)
	if !ok {
		return
	}
	status, err := service.StopMiner(r.Context())
	if err != nil {
		s.writeServiceError(w, err, "miner_stop_failed", "BTC09 could not stop the miner.")
		return
	}
	s.writeData(w, http.StatusOK, status)
}

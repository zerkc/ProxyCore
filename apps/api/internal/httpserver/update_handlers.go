package httpserver

import (
	"net/http"

	"github.com/zerkc/ProxyCore/apps/api/internal/update"
	"github.com/zerkc/ProxyCore/apps/api/internal/version"
)

func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		writeJSON(w, http.StatusOK, update.Result{
			Status:         update.StatusDisabled,
			CurrentVersion: version.Version,
		})
		return
	}
	writeJSON(w, http.StatusOK, s.updates.Check(r.Context()))
}

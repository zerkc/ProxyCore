package httpserver

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// httpError carries an explicit status for configuration routes.
type httpError struct {
	status  int
	message string
}

func (e *httpError) Error() string { return e.message }

// writeConfigError renders {error} with the appropriate status, matching Node.
func writeConfigError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if he, ok := err.(*httpError); ok {
		status = he.status
	}
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func (s *Server) configStore(w http.ResponseWriter) (*configuration.Store, bool) {
	if s.config == nil {
		writeConfigError(w, &httpError{status: http.StatusServiceUnavailable, message: "configuration store is not configured"})
		return nil, false
	}
	return s.config, true
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	status, err := store.Status(r.Context())
	if err != nil {
		writeConfigError(w, err)
		return
	}
	publicCerts := make([]map[string]any, 0, len(status.Certificates))
	for _, cert := range status.Certificates {
		publicCerts = append(publicCerts, cert.PublicCertificate())
	}
	response := map[string]any{
		"jobs":         status.Jobs,
		"settings":     status.Settings,
		"zones":        status.Zones,
		"streams":      status.Streams,
		"certificates": publicCerts,
	}
	if status.DesiredRevision != nil {
		response["desiredRevision"] = map[string]any{
			"id":             status.DesiredRevision.ID,
			"revisionNumber": status.DesiredRevision.RevisionNumber,
			"checksum":       status.DesiredRevision.Checksum,
			"createdAt":      status.DesiredRevision.CreatedAt,
		}
	}
	if status.AppliedRevision != nil {
		applied := map[string]any{
			"id":             status.AppliedRevision.ID,
			"revisionNumber": status.AppliedRevision.RevisionNumber,
			"checksum":       status.AppliedRevision.Checksum,
		}
		if status.AppliedRevision.AppliedAt != nil {
			applied["appliedAt"] = status.AppliedRevision.AppliedAt
		}
		response["appliedRevision"] = applied
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	settings, err := store.GetSettings(r.Context())
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	body, ok := decodeJSONObject(w, r)
	if !ok {
		return
	}
	patch := configuration.SettingsPatch{}
	if ingress, ok := asObject(body["ingress"]); ok {
		patch.Ingress = &domain.Ingress{IPv4: readString(ingress, "ipv4"), IPv6: readString(ingress, "ipv6")}
	}
	if pool, ok := asObject(body["defaultPool"]); ok {
		id := readString(pool, "id")
		if id == "" {
			id = "default"
		}
		endpoints := []domain.ResolverEndpoint{}
		if err := decodeInto(pool["endpoints"], &endpoints); err != nil {
			writeConfigError(w, err)
			return
		}
		patch.DefaultPool = &domain.ResolverPool{ID: id, Endpoints: endpoints}
	}
	if rules, ok := body["forwardingRules"].([]any); ok {
		parsed := []domain.ForwardingRule{}
		if err := decodeInto(rules, &parsed); err != nil {
			writeConfigError(w, err)
			return
		}
		patch.ForwardingRules = parsed
	}
	if value, ok := readNumber(body, "retentionMaxAgeDays"); ok {
		patch.RetentionMaxAgeDays = &value
	}
	if value, ok := readNumber(body, "retentionMaxSizeMb"); ok {
		patch.RetentionMaxSizeMb = &value
	}
	settings, err := store.UpdateSettings(r.Context(), patch)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	apply, err := store.CreateApplyJob(r.Context(), user.ID)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, apply)
}

func (s *Server) handleListZones(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	zones, err := store.ListZones(r.Context())
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"zones": zones})
}

func (s *Server) handleCreateZone(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	body, ok := decodeJSONObject(w, r)
	if !ok {
		return
	}
	name, isString := body["name"].(string)
	if !isString {
		writeConfigError(w, &httpError{status: http.StatusBadRequest, message: "Zone name is required"})
		return
	}
	result, err := store.CreateZone(r.Context(), name, user.ID)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"zone": result.Value, "apply": result.Apply})
}

func (s *Server) handleGetZoneRecords(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	zone, err := store.GetZone(r.Context(), r.PathValue("zoneId"))
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"zone": zone})
}

func (s *Server) handleAddRecord(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	body, ok := decodeJSONObject(w, r)
	if !ok {
		return
	}
	record, err := parseRecordMutationBody(body)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	result, err := store.AddRecord(r.Context(), r.PathValue("zoneId"), record, user.ID)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"record": result.Value, "apply": result.Apply})
}

func (s *Server) handlePatchRecord(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	zoneID := r.PathValue("zoneId")
	recordID := r.PathValue("recordId")
	body, ok := decodeJSONObject(w, r)
	if !ok {
		return
	}
	body["id"] = recordID
	record, err := parseRecordMutationBody(body)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	zone, err := store.GetZone(r.Context(), zoneID)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	found := false
	for _, item := range zone.Records {
		if item.ID == recordID {
			found = true
			break
		}
	}
	if !found {
		writeConfigError(w, &httpError{status: http.StatusNotFound, message: "Record not found"})
		return
	}
	record.ID = recordID
	result, err := store.AddRecord(r.Context(), zoneID, record, user.ID)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"record": result.Value, "apply": result.Apply})
}

func (s *Server) handleListStreams(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	streams, err := store.ListStreams(r.Context())
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"streams": streams})
}

func (s *Server) handleCreateStream(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	body, ok := decodeJSONObject(w, r)
	if !ok {
		return
	}
	route, err := parseStreamBody(body, "")
	if err != nil {
		writeConfigError(w, err)
		return
	}
	stream, err := store.AddStream(r.Context(), route)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"stream": stream})
}

func (s *Server) handlePatchStream(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	streamID := r.PathValue("streamId")
	streams, err := store.ListStreams(r.Context())
	if err != nil {
		writeConfigError(w, err)
		return
	}
	var existing *domain.StreamRoute
	for i := range streams {
		if streams[i].ID == streamID {
			existing = &streams[i]
			break
		}
	}
	if existing == nil {
		writeConfigError(w, &httpError{status: http.StatusNotFound, message: "Stream not found"})
		return
	}
	body, ok := decodeJSONObject(w, r)
	if !ok {
		return
	}
	route, err := mergeStreamBody(body, *existing)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	stream, err := store.AddStream(r.Context(), route)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stream": stream})
}

func (s *Server) handleDeleteStream(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	streamID := r.PathValue("streamId")
	streams, err := store.ListStreams(r.Context())
	if err != nil {
		writeConfigError(w, err)
		return
	}
	found := false
	for _, stream := range streams {
		if stream.ID == streamID {
			found = true
			break
		}
	}
	if !found {
		writeConfigError(w, &httpError{status: http.StatusNotFound, message: "Stream not found"})
		return
	}
	if err := store.DeleteStream(r.Context(), streamID); err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAcmeChallenge(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	keyAuthorization, ok := globalHTTP01Get(token)
	w.Header().Set("cache-control", "no-store")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Not found"))
		return
	}
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(keyAuthorization))
}

// ---- request parsing helpers (shared) ----

func decodeJSONObject(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	object, err := readJSONObject(w, r)
	if err != nil {
		writeConfigError(w, err)
		return nil, false
	}
	return object, true
}

// readJSONObject decodes a JSON object body without writing a response, so
// callers can compose it with further validation (ports Node readJson).
func readJSONObject(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	var raw any
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&raw); err != nil {
		return nil, &httpError{status: http.StatusBadRequest, message: "JSON object body is required"}
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, &httpError{status: http.StatusBadRequest, message: "JSON object body is required"}
	}
	return object, nil
}

func asObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok && object != nil
}

func readString(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func readNumber(object map[string]any, key string) (int, bool) {
	if value, ok := object[key].(float64); ok {
		return int(value), true
	}
	return 0, false
}

func decodeInto(value any, dest any) error {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, dest)
}

func inferRequestIngress(r *http.Request) domain.Ingress {
	rawHost := strings.TrimSpace(r.Host)
	if rawHost == "" {
		return domain.Ingress{}
	}
	hostname := rawHost
	if strings.HasPrefix(rawHost, "[") {
		if end := strings.Index(rawHost, "]"); end > 0 {
			hostname = rawHost[1:end]
		}
	} else if strings.Count(rawHost, ":") == 1 {
		hostname = strings.SplitN(rawHost, ":", 2)[0]
	}

	if ipKind(hostname) == 4 && !isLoopbackIPv4(hostname) && hostname != "0.0.0.0" && isPrivateIPv4(hostname) {
		return domain.Ingress{IPv4: hostname}
	}
	if ipKind(hostname) == 6 && hostname != "::" && hostname != "::1" && isLanIPv6(hostname) {
		return domain.Ingress{IPv6: hostname}
	}
	return domain.Ingress{}
}

func ipKind(value string) int {
	ip := net.ParseIP(value)
	if ip == nil {
		return 0
	}
	if strings.Contains(value, ":") {
		return 6
	}
	return 4
}

func isLoopbackIPv4(address string) bool {
	return address == "127.0.0.1" || strings.HasPrefix(address, "127.")
}

func isPrivateIPv4(address string) bool {
	octets := strings.Split(address, ".")
	if len(octets) != 4 {
		return false
	}
	first := atoiSafe(octets[0])
	second := atoiSafe(octets[1])
	return first == 10 || (first == 172 && second >= 16 && second <= 31) || (first == 192 && second == 168)
}

func isLanIPv6(address string) bool {
	lower := strings.ToLower(address)
	return strings.HasPrefix(lower, "fc") || strings.HasPrefix(lower, "fd")
}

func atoiSafe(value string) int {
	result := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return -1
		}
		result = result*10 + int(r-'0')
	}
	return result
}

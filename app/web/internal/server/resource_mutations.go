package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"

	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// maxResourceName is the frozen name length for every Stage C resource.
const maxResourceName = 64

// repositoryRequest is the create/update body. PUT replaces every mutable field;
// an omitted password keeps the stored secret, so rotation is explicit.
type repositoryRequest struct {
	Name     string      `json:"name"`
	Kind     string      `json:"kind"`
	TLSMode  string      `json:"tls_mode"`
	Root     string      `json:"root"`
	FTP      *ftpRequest `json:"ftp"`
	Password *string     `json:"password"`
}

type ftpRequest struct {
	Host     string `json:"host"`
	Port     int64  `json:"port"`
	BasePath string `json:"base_path"`
	Username string `json:"username"`
}

type sourceRequest struct {
	Name     string  `json:"name"`
	Socket   string  `json:"socket"`
	Datadir  string  `json:"datadir"`
	Username string  `json:"username"`
	Password *string `json:"password"`
}

type policyRequest struct {
	Name                string  `json:"name"`
	SourceID            string  `json:"source_id"`
	LocalRepositoryID   string  `json:"local_repository_id"`
	ReplicaRepositoryID *string `json:"replica_repository_id"`
	FullCron            string  `json:"full_cron"`
	IncrementalCron     string  `json:"incremental_cron"`
	Timezone            string  `json:"timezone"`
	MaxChainLength      int64   `json:"max_chain_length"`
	Parallel            int64   `json:"parallel"`
	Throttle            int64   `json:"throttle"`
	ZstdLevel           int64   `json:"zstd_level"`
	PageTracking        bool    `json:"page_tracking"`
	Enabled             bool    `json:"enabled"`
}

func (h resourceHandlers) createRepository(c *gin.Context) {
	var request repositoryRequest
	if !render.DecodeJSON(c, &request) {
		return
	}
	in, plaintextFTP, err := h.repositoryInput(request, true)
	if err != nil {
		repositoryValidationError(c, err)
		return
	}
	repository, err := h.database.CreateRepository(c.Request.Context(), in, h.sealer(),
		middleware.SuccessAudit(c, "repositories.create", http.StatusCreated, "repository", "",
			store.AuditDetail{PlaintextFTP: plaintextFTP}))
	if err != nil {
		h.resourceWriteError(c, err, "Repository")
		return
	}
	middleware.AuditResource(c, "repository", repository.ID, store.AuditDetail{PlaintextFTP: plaintextFTP})
	render.JSON(c, http.StatusCreated, repository)
	if c.Writer.Status() < 500 {
		middleware.MarkAuditRecorded(c)
	}
}

func (h resourceHandlers) updateRepository(c *gin.Context) {
	var request repositoryRequest
	if !render.DecodeJSON(c, &request) {
		return
	}
	id := c.Param("id")
	stored, err := h.database.Repository(c.Request.Context(), id)
	if err != nil {
		h.resourceError(c, err, "Repository")
		return
	}
	if request.Kind != stored.Kind || request.TLSMode != stored.TLSMode {
		render.Error(c, http.StatusUnprocessableEntity, render.ValidationFailed, "kind and tls_mode cannot be changed")
		return
	}
	if err = repositoryConfigShape(stored.Kind, request); err != nil {
		render.Error(c, http.StatusUnprocessableEntity, render.ValidationFailed, err.Error())
		return
	}
	in, plaintextFTP, err := h.repositoryInput(request, false)
	if err != nil {
		repositoryValidationError(c, err)
		return
	}
	repository, err := h.database.UpdateRepository(c.Request.Context(), id, in, h.sealer(),
		middleware.SuccessAudit(c, "repositories.update", http.StatusOK, "repository", id,
			store.AuditDetail{PlaintextFTP: plaintextFTP}))
	if err != nil {
		h.resourceWriteError(c, err, "Repository")
		return
	}
	middleware.AuditResource(c, "repository", repository.ID, store.AuditDetail{PlaintextFTP: plaintextFTP})
	render.JSON(c, http.StatusOK, repository)
	if c.Writer.Status() < 500 {
		middleware.MarkAuditRecorded(c)
	}
}

// repositoryConfigShape refuses fields that belong to the other kind. PUT uses
// this after reading the stored row so a local-shaped body cannot be written
// onto an FTP repository, and the reverse cannot 500 while decoding.
func repositoryConfigShape(kind string, request repositoryRequest) error {
	switch kind {
	case store.RepositoryLocal:
		if request.FTP != nil || request.Password != nil {
			return errors.New("a local repository has no FTP configuration or password")
		}
	case store.RepositoryFTP:
		if request.Root != "" {
			return errors.New("an FTP repository has no root")
		}
	}
	return nil
}

// repositoryInput validates one repository body and applies the frozen tls_mode
// rule: a local repository is "none"; an FTP repository must send "plain"
// explicitly. An omitted value never becomes plain, and the FTPS values
// ("explicit", "implicit") are refused because FTPS is not implemented in W3.
func (h resourceHandlers) repositoryInput(request repositoryRequest, creating bool) (store.RepositoryInput, bool, error) {
	in := store.RepositoryInput{Name: request.Name, Kind: request.Kind, TLSMode: request.TLSMode}
	if err := validateResourceName(request.Name); err != nil {
		return in, false, err
	}
	switch request.Kind {
	case store.RepositoryLocal:
		err := h.localRepositoryInput(&in, request)
		return in, false, err
	case store.RepositoryFTP:
		err := h.ftpRepositoryInput(&in, request, creating)
		return in, err == nil, err
	default:
		return in, false, errors.New(`kind must be "local" or "ftp"`)
	}
}

// localRepositoryInput fills a local repository. It owns one absolute directory
// and never carries an FTP configuration or a secret.
func (h resourceHandlers) localRepositoryInput(in *store.RepositoryInput, request repositoryRequest) error {
	if request.TLSMode != store.TLSModeNone {
		return &tlsModeError{`a local repository requires tls_mode "none"`}
	}
	if !isAbsolutePath(request.Root) {
		return errors.New("root must be a non-empty absolute path")
	}
	if request.FTP != nil || request.Password != nil {
		return errors.New("a local repository has no FTP configuration or password")
	}
	if err := h.service.Settings.ValidateRepositoryRoot(request.Root); err != nil {
		return errors.New("root must not contain the credential key file")
	}
	in.Root = request.Root
	return nil
}

// ftpRepositoryInput fills a plain FTP repository. TLSMode must be exactly
// "plain"; the password is required on create and optional on update, where an
// omitted value keeps the stored secret.
func (h resourceHandlers) ftpRepositoryInput(in *store.RepositoryInput, request repositoryRequest, creating bool) error {
	if request.TLSMode != store.TLSModePlain {
		return &tlsModeError{`an FTP repository requires tls_mode "plain" explicitly; FTPS is not supported`}
	}
	if request.Root != "" {
		return errors.New("an FTP repository has no root")
	}
	config, err := validateFTP(request.FTP)
	if err != nil {
		return err
	}
	in.FTP = config
	if request.Password == nil {
		if creating {
			return errors.New("password is required for an FTP repository")
		}
		return nil
	}
	if err = validateSecret(*request.Password); err != nil {
		return err
	}
	in.Secret = []byte(*request.Password)
	return nil
}

func validateFTP(config *ftpRequest) (*store.FTPRepositoryConfig, error) {
	if config == nil {
		return nil, errors.New("an FTP repository requires host, port, base_path and username")
	}
	if config.Host == "" || strings.ContainsAny(config.Host, "\x00\r\n") {
		return nil, errors.New("ftp.host must be a non-empty host or IP address without control bytes")
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, errors.New("ftp.port must be between 1 and 65535")
	}
	if !isAbsoluteFTPPath(config.BasePath) {
		return nil, errors.New("ftp.base_path must be an absolute, clean slash-separated path")
	}
	if config.Username == "" || strings.ContainsAny(config.Username, "\x00\r\n") {
		return nil, errors.New("ftp.username must be a non-empty string without control bytes")
	}
	return &store.FTPRepositoryConfig{
		Host: config.Host, Port: config.Port, BasePath: config.BasePath, Username: config.Username,
	}, nil
}

func (h resourceHandlers) deleteRepository(c *gin.Context) {
	id := c.Param("id")
	middleware.AuditResource(c, "repository", id, store.AuditDetail{})
	if err := h.database.DeleteRepository(c.Request.Context(), id,
		middleware.SuccessAudit(c, "repositories.delete", http.StatusNoContent, "repository", id, store.AuditDetail{})); err != nil {
		h.resourceError(c, err, "Repository")
		return
	}
	c.Status(http.StatusNoContent)
	middleware.MarkAuditRecorded(c)
}

type probeResponse struct {
	OK    bool              `json:"ok"`
	Error *probeFailureBody `json:"error,omitempty"`
}

type probeFailureBody struct {
	Code      string `json:"code"`
	ReplyCode int    `json:"reply_code"`
}

func (h resourceHandlers) probeRepository(c *gin.Context) {
	outcome, err := h.service.ProbeRepository(c.Request.Context(), c.Param("id"))
	switch {
	case isUnavailable(err):
		unavailableDependency(c)
		return
	case errors.Is(err, store.ErrNotFound):
		render.Error(c, http.StatusNotFound, render.NotFound, "Repository not found")
		return
	case err != nil:
		internalError(c)
		return
	}
	if outcome.Failure != nil {
		middleware.RecordOutcome(c, h.database, h.logger, "failed", http.StatusOK,
			"repositories.probe", "repository", c.Param("id"), store.AuditDetail{})
		render.JSON(c, http.StatusOK, probeResponse{OK: false, Error: &probeFailureBody{
			Code: outcome.Failure.Code, ReplyCode: outcome.Failure.ReplyCode,
		}})
		return
	}
	render.JSON(c, http.StatusOK, probeResponse{OK: true})
}

func (h resourceHandlers) createSource(c *gin.Context) {
	var request sourceRequest
	if !render.DecodeJSON(c, &request) {
		return
	}
	in, err := validateSource(request, true)
	if err != nil {
		validationError(c, err)
		return
	}
	source, err := h.database.CreateSource(c.Request.Context(), in, h.sealer(),
		middleware.SuccessAudit(c, "sources.create", http.StatusCreated, "source", "", store.AuditDetail{}))
	if err != nil {
		h.resourceWriteError(c, err, "Source")
		return
	}
	middleware.AuditResource(c, "source", source.ID, store.AuditDetail{})
	render.JSON(c, http.StatusCreated, source)
	if c.Writer.Status() < 500 {
		middleware.MarkAuditRecorded(c)
	}
}

func (h resourceHandlers) updateSource(c *gin.Context) {
	var request sourceRequest
	if !render.DecodeJSON(c, &request) {
		return
	}
	id := c.Param("id")
	in, err := validateSource(request, false)
	if err != nil {
		validationError(c, err)
		return
	}
	source, err := h.database.UpdateSource(c.Request.Context(), id, in, h.sealer(),
		middleware.SuccessAudit(c, "sources.update", http.StatusOK, "source", id, store.AuditDetail{}))
	if err != nil {
		h.resourceWriteError(c, err, "Source")
		return
	}
	middleware.AuditResource(c, "source", source.ID, store.AuditDetail{})
	render.JSON(c, http.StatusOK, source)
	if c.Writer.Status() < 500 {
		middleware.MarkAuditRecorded(c)
	}
}

func validateSource(request sourceRequest, creating bool) (store.SourceInput, error) {
	in := store.SourceInput{Name: request.Name, Socket: request.Socket, Datadir: request.Datadir, Username: request.Username}
	if err := validateResourceName(request.Name); err != nil {
		return in, err
	}
	if !isAbsolutePath(request.Socket) {
		return in, errors.New("socket must be a non-empty absolute path")
	}
	if !isAbsolutePath(request.Datadir) {
		return in, errors.New("datadir must be a non-empty absolute path")
	}
	if request.Username == "" || strings.ContainsAny(request.Username, "\x00\r\n") {
		return in, errors.New("username must be a non-empty string without control bytes")
	}
	if request.Password != nil {
		if err := validateSecret(*request.Password); err != nil {
			return in, err
		}
		in.Secret = []byte(*request.Password)
	} else if creating {
		return in, errors.New("password is required for a source")
	}
	return in, nil
}

// tlsModeError marks the frozen tls_mode shape violation, which is reported as
// 422 rather than 400: an FTP repository must send "plain" explicitly, and a
// local repository must send "none".
type tlsModeError struct{ message string }

func (e *tlsModeError) Error() string { return e.message }

// repositoryValidationError reports a tls_mode violation as 422 validation_failed
// and every other repository body error as 400.
func repositoryValidationError(c *gin.Context, err error) {
	var mode *tlsModeError
	if errors.As(err, &mode) {
		render.Error(c, http.StatusUnprocessableEntity, render.ValidationFailed, mode.Error())
		return
	}
	validationError(c, err)
}

func validateSecret(secret string) error {
	if secret == "" || strings.ContainsRune(secret, '\x00') {
		return errors.New("password must be a non-empty string without NUL bytes")
	}
	return nil
}

func (h resourceHandlers) deleteSource(c *gin.Context) {
	id := c.Param("id")
	middleware.AuditResource(c, "source", id, store.AuditDetail{})
	if err := h.database.DeleteSource(c.Request.Context(), id,
		middleware.SuccessAudit(c, "sources.delete", http.StatusNoContent, "source", id, store.AuditDetail{})); err != nil {
		h.resourceError(c, err, "Source")
		return
	}
	c.Status(http.StatusNoContent)
	middleware.MarkAuditRecorded(c)
}

type preflightResponse struct {
	OK       bool                      `json:"ok"`
	Checks   []string                  `json:"checks"`
	Warnings []store.PreflightWarning  `json:"warnings"`
	Error    *preflightFailureResponse `json:"error,omitempty"`
}

type preflightFailureResponse struct {
	Check   string   `json:"check"`
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Missing []string `json:"missing,omitempty"`
}

func (h resourceHandlers) preflightSource(c *gin.Context) {
	var request struct {
		LocalRepositoryID string `json:"local_repository_id"`
	}
	if !render.DecodeJSON(c, &request) {
		return
	}
	if request.LocalRepositoryID == "" {
		validationError(c, errors.New("local_repository_id is required"))
		return
	}
	source, err := h.database.Source(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.resourceError(c, err, "Source")
		return
	}
	outcome, err := h.service.PreflightSource(c.Request.Context(), source.ID, request.LocalRepositoryID)
	switch {
	case isUnavailable(err):
		unavailableDependency(c)
		return
	case errors.Is(err, resources.ErrLocalRepositoryRequired):
		validationError(c, errors.New("local_repository_id must name a local repository"))
		return
	case err != nil:
		h.resourceError(c, err, "Source")
		return
	}
	if !h.persistSourcePreflight(c, source.ID, outcome) {
		return
	}
	render.JSON(c, http.StatusOK, preflightBody(outcome))
}

// persistSourcePreflight writes last_preflight_json for every completed run.
// Server identity is stored only when the identity check succeeded. A failed
// run keeps the 200 body and records a failed audit instead of a success audit.
func (h resourceHandlers) persistSourcePreflight(c *gin.Context, sourceID string, outcome resources.PreflightOutcome) bool {
	record := outcome.Record()
	if outcome.Failure != nil && outcome.Failure.Check == "identity" {
		record.ServerUUID = ""
		record.ServerVersion = ""
	}
	entry := store.AuditLog{}
	if outcome.OK() {
		entry = middleware.SuccessAudit(c, "sources.preflight", http.StatusOK, "source", sourceID, store.AuditDetail{})
	}
	if _, err := h.database.RecordSourcePreflight(c.Request.Context(), sourceID, record, entry); err != nil {
		h.resourceError(c, err, "Source")
		return false
	}
	if outcome.OK() {
		middleware.AuditResource(c, "source", sourceID, store.AuditDetail{})
		middleware.MarkAuditRecorded(c)
		return true
	}
	middleware.RecordOutcome(c, h.database, h.logger, "failed", http.StatusOK,
		"sources.preflight", "source", sourceID, store.AuditDetail{})
	return true
}

func preflightBody(outcome resources.PreflightOutcome) preflightResponse {
	record := outcome.Record()
	body := preflightResponse{OK: record.OK, Checks: record.Checks, Warnings: record.Warnings}
	if outcome.Failure != nil {
		body.Error = &preflightFailureResponse{
			Check: outcome.Failure.Check, Code: outcome.Failure.Code,
			Message: outcome.Failure.Message, Missing: outcome.Failure.Missing,
		}
	}
	return body
}

func (h resourceHandlers) createPolicy(c *gin.Context) {
	var request policyRequest
	if !render.DecodeJSON(c, &request) {
		return
	}
	in, err := h.policyInput(c, request)
	if err != nil {
		validationError(c, err)
		return
	}
	if !h.authoriseEnable(c, &in) {
		return
	}
	policy, err := h.database.CreatePolicy(c.Request.Context(), in,
		middleware.SuccessAudit(c, "policies.create", http.StatusCreated, "policy", "", store.AuditDetail{}))
	if err != nil {
		h.resourceWriteError(c, err, "Policy")
		return
	}
	middleware.AuditResource(c, "policy", policy.ID, store.AuditDetail{})
	render.JSON(c, http.StatusCreated, policy)
	if c.Writer.Status() < 500 {
		middleware.MarkAuditRecorded(c)
	}
}

func (h resourceHandlers) updatePolicy(c *gin.Context) {
	var request policyRequest
	if !render.DecodeJSON(c, &request) {
		return
	}
	id := c.Param("id")
	in, err := h.policyInput(c, request)
	if err != nil {
		validationError(c, err)
		return
	}
	if !h.authoriseEnable(c, &in) {
		return
	}
	policy, err := h.database.UpdatePolicy(c.Request.Context(), id, in,
		middleware.SuccessAudit(c, "policies.update", http.StatusOK, "policy", id, store.AuditDetail{}))
	if err != nil {
		h.resourceWriteError(c, err, "Policy")
		return
	}
	middleware.AuditResource(c, "policy", policy.ID, store.AuditDetail{})
	render.JSON(c, http.StatusOK, policy)
	if c.Writer.Status() < 500 {
		middleware.MarkAuditRecorded(c)
	}
}

func (h resourceHandlers) enablePolicy(c *gin.Context) {
	id := c.Param("id")
	policy, err := h.database.Policy(c.Request.Context(), id)
	if err != nil {
		h.resourceError(c, err, "Policy")
		return
	}
	outcome, err := h.service.PolicyPreflight(c.Request.Context(), policy)
	switch {
	case isUnavailable(err):
		unavailableDependency(c)
		return
	case err != nil:
		h.resourceError(c, err, "Policy")
		return
	}
	if !outcome.OK() {
		render.ErrorPreflight(c, http.StatusConflict, "Policy preflight failed; the policy stays disabled", preflightRender(outcome))
		return
	}
	record := outcome.Record()
	updated, err := h.database.EnablePolicy(c.Request.Context(), id, true, &record,
		middleware.SuccessAudit(c, "policies.enable", http.StatusOK, "policy", id, store.AuditDetail{}))
	if err != nil {
		h.resourceError(c, err, "Policy")
		return
	}
	middleware.AuditResource(c, "policy", updated.ID, store.AuditDetail{})
	render.JSON(c, http.StatusOK, updated)
	if c.Writer.Status() < 500 {
		middleware.MarkAuditRecorded(c)
	}
}

func (h resourceHandlers) disablePolicy(c *gin.Context) {
	id := c.Param("id")
	middleware.AuditResource(c, "policy", id, store.AuditDetail{})
	// Disable needs no preflight: a broken source must always be stoppable.
	updated, err := h.database.EnablePolicy(c.Request.Context(), id, false, nil,
		middleware.SuccessAudit(c, "policies.disable", http.StatusOK, "policy", id, store.AuditDetail{}))
	if err != nil {
		h.resourceError(c, err, "Policy")
		return
	}
	render.JSON(c, http.StatusOK, updated)
	if c.Writer.Status() < 500 {
		middleware.MarkAuditRecorded(c)
	}
}

func (h resourceHandlers) deletePolicy(c *gin.Context) {
	id := c.Param("id")
	middleware.AuditResource(c, "policy", id, store.AuditDetail{})
	if err := h.database.DeletePolicy(c.Request.Context(), id,
		middleware.SuccessAudit(c, "policies.delete", http.StatusNoContent, "policy", id, store.AuditDetail{})); err != nil {
		h.resourceError(c, err, "Policy")
		return
	}
	c.Status(http.StatusNoContent)
	middleware.MarkAuditRecorded(c)
}

// authoriseEnable runs the preflight a requested enable requires. A policy is
// enabled only after every check passed: the source preflight against its local
// repository, plus the replica probe when one is configured. A failed preflight
// refuses the whole request and writes nothing, so a create leaves no row and an
// update keeps the previous row, including an already-enabled policy and its
// settings. The caller then reports 409 preflight_failed.
func (h resourceHandlers) authoriseEnable(c *gin.Context, in *store.PolicyInput) bool {
	if !in.Enabled {
		return true
	}
	if !h.service.Settings.Preflight() {
		unavailableDependency(c)
		return false
	}
	policy := store.Policy{
		SourceID: in.SourceID, LocalRepositoryID: in.LocalRepositoryID, ReplicaRepositoryID: in.ReplicaRepositoryID,
	}
	outcome, err := h.service.PolicyPreflight(c.Request.Context(), policy)
	switch {
	case isUnavailable(err):
		unavailableDependency(c)
		return false
	case errors.Is(err, store.ErrNotFound):
		render.Error(c, http.StatusNotFound, render.NotFound, "Referenced source or repository not found")
		return false
	case err != nil:
		internalError(c)
		return false
	}
	if !outcome.OK() {
		render.ErrorPreflight(c, http.StatusConflict, "Policy preflight failed; the policy is not enabled", preflightRender(outcome))
		return false
	}
	record := outcome.Record()
	in.Preflight = &record
	return true
}

func (h resourceHandlers) policyInput(c *gin.Context, request policyRequest) (store.PolicyInput, error) {
	in := store.PolicyInput{
		Name: request.Name, SourceID: request.SourceID, LocalRepositoryID: request.LocalRepositoryID,
		ReplicaRepositoryID: request.ReplicaRepositoryID, FullCron: request.FullCron,
		IncrementalCron: request.IncrementalCron, Timezone: request.Timezone,
		MaxChainLength: request.MaxChainLength, Parallel: request.Parallel, Throttle: request.Throttle,
		ZstdLevel: request.ZstdLevel, PageTracking: request.PageTracking, Enabled: request.Enabled,
	}
	if err := validatePolicy(request); err != nil {
		return in, err
	}
	if err := h.validatePolicyReferences(c, request); err != nil {
		return in, err
	}
	return in, nil
}

func validatePolicy(request policyRequest) error {
	if err := validateResourceName(request.Name); err != nil {
		return err
	}
	if err := validatePolicySchedule(request); err != nil {
		return err
	}
	return validatePolicyTuning(request)
}

// validatePolicySchedule checks the two standard cron expressions and the IANA
// time zone. Descriptors such as "@every" are not accepted.
func validatePolicySchedule(request policyRequest) error {
	if request.SourceID == "" || request.LocalRepositoryID == "" {
		return errors.New("source_id and local_repository_id are required")
	}
	if request.ReplicaRepositoryID != nil && *request.ReplicaRepositoryID == request.LocalRepositoryID {
		return errors.New("replica_repository_id must differ from local_repository_id")
	}
	for name, expression := range map[string]string{
		"full_cron": request.FullCron, "incremental_cron": request.IncrementalCron,
	} {
		if _, err := cron.ParseStandard(expression); err != nil {
			return errors.New(name + " must be a standard 5-field cron expression")
		}
	}
	return validateTimezone(request.Timezone)
}

func validateTimezone(name string) error {
	if name == "" || name == "Local" {
		return errors.New("timezone must be a non-empty IANA time zone name")
	}
	location, err := time.LoadLocation(name)
	if err != nil || location == time.Local {
		return errors.New("timezone must be a non-empty IANA time zone name")
	}
	return nil
}

// validatePolicyTuning applies the frozen numeric ranges.
func validatePolicyTuning(request policyRequest) error {
	switch {
	case request.MaxChainLength < 1:
		return errors.New("max_chain_length must be at least 1")
	case request.Parallel < 1:
		return errors.New("parallel must be at least 1")
	case request.Throttle < 0:
		return errors.New("throttle must not be negative")
	case request.ZstdLevel < 1 || request.ZstdLevel > 22:
		return errors.New("zstd_level must be between 1 and 22")
	default:
		return nil
	}
}

// validatePolicyReferences checks the referenced rows and their kinds: the local
// repository must be local and the replica repository, when present, FTP.
func (h resourceHandlers) validatePolicyReferences(c *gin.Context, request policyRequest) error {
	ctx := c.Request.Context()
	if _, err := h.database.Source(ctx, request.SourceID); err != nil {
		return referenceError(err, "source_id")
	}
	local, err := h.database.Repository(ctx, request.LocalRepositoryID)
	if err != nil {
		return referenceError(err, "local_repository_id")
	}
	if local.Kind != store.RepositoryLocal {
		return errors.New("local_repository_id must name a local repository")
	}
	if request.ReplicaRepositoryID == nil {
		return nil
	}
	replica, err := h.database.Repository(ctx, *request.ReplicaRepositoryID)
	if err != nil {
		return referenceError(err, "replica_repository_id")
	}
	if replica.Kind != store.RepositoryFTP {
		return errors.New("replica_repository_id must name an FTP repository")
	}
	return nil
}

func referenceError(err error, field string) error {
	if errors.Is(err, store.ErrNotFound) {
		return errors.New(field + " does not name an existing resource")
	}
	return err
}

func (h resourceHandlers) sealer() store.SecretSealer {
	if h.service == nil || !h.service.Settings.Credentials() {
		return nil
	}
	return h.service.Settings.Sealer
}

func (h resourceHandlers) resourceWriteError(c *gin.Context, err error, name string) {
	if isUnavailable(err) {
		unavailableDependency(c)
		return
	}
	h.resourceError(c, err, name)
}

func isUnavailable(err error) bool {
	return errors.Is(err, resources.ErrUnavailable) || errors.Is(err, store.ErrUnavailable)
}

// unavailableDependency is the fixed response for an operation whose deployment
// inputs are missing. It never names the absent path or key.
func unavailableDependency(c *gin.Context) {
	render.Error(c, http.StatusServiceUnavailable, render.Unavailable, "This operation is not available in this deployment")
}

func preflightRender(outcome resources.PreflightOutcome) render.Preflight {
	record := outcome.Record()
	body := render.Preflight{OK: record.OK, Checks: record.Checks, Warnings: []render.PreflightWarning{}}
	for _, warning := range record.Warnings {
		body.Warnings = append(body.Warnings, render.PreflightWarning{Code: warning.Code, Message: warning.Message})
	}
	if outcome.Failure != nil {
		body.Error = &render.PreflightError{
			Check: outcome.Failure.Check, Code: outcome.Failure.Code,
			Message: outcome.Failure.Message, Missing: outcome.Failure.Missing,
		}
	}
	return body
}

func validateResourceName(name string) error {
	if name == "" || len([]rune(name)) > maxResourceName || strings.TrimSpace(name) != name {
		return errors.New("name must be 1 to 64 characters with no leading or trailing spaces")
	}
	for _, character := range name {
		if character < 0x20 || character == 0x7f {
			return errors.New("name must not contain control characters")
		}
	}
	return nil
}

func isAbsolutePath(path string) bool {
	return strings.HasPrefix(path, "/") && !strings.ContainsAny(path, "\x00\r\n")
}

func isAbsoluteFTPPath(path string) bool {
	if !isAbsolutePath(path) {
		return false
	}
	if path == "/" {
		return true
	}
	if strings.HasSuffix(path, "/") || strings.Contains(path, "//") || strings.Contains(path, "\\") {
		return false
	}
	return isCleanFTPPath(path)
}

func isCleanFTPPath(path string) bool {
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" || part == "." || part == ".." || strings.TrimSpace(part) != part {
			return false
		}
	}
	return true
}

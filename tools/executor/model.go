package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"time"
)

// Action types from the contract. rollback is recognised but never run (see policy.go).
const (
	actionRestart     = "restart"
	actionScale       = "scale"
	actionRollback    = "rollback"
	actionRemoveFault = "remove-fault"
)

// Status is a proposal's lifecycle state. pending and approved are open; the rest are final.
type Status string

const (
	statusPending  Status = "pending"
	statusApproved Status = "approved" // approved and running; becomes executed, failed or blocked
	statusRejected Status = "rejected"
	statusExecuted Status = "executed"
	statusFailed   Status = "failed"
	statusExpired  Status = "expired"
	statusBlocked  Status = "blocked"
)

func (s Status) open() bool { return s == statusPending || s == statusApproved }

// Reasons in 4xx bodies, audit lines and the GET response. The first four are the contract's.
const (
	reasonNotAllowed          = "not-allowed"
	reasonOutOfBounds         = "out-of-bounds"
	reasonAlreadyActed        = "already-acted"
	reasonKillSwitch          = "kill-switch"
	reasonInvalid             = "invalid-request"
	reasonUnauthorized        = "unauthorized"
	reasonNotFound            = "not-found"
	reasonApprovalUnavailable = "approval-unavailable"
	reasonExpired             = "expired"
	reasonExecError           = "execution-error"
	reasonInterrupted         = "interrupted"
	reasonInternal            = "internal"
)

// Audit events (contract), plus approval_denied for clicks by someone who may not decide.
const (
	eventReceived       = "proposal_received"
	eventApproved       = "approved"
	eventRejected       = "rejected"
	eventExecuted       = "executed"
	eventBlocked        = "blocked"
	eventFailed         = "failed"
	eventApprovalDenied = "approval_denied"
)

// Action is what the agent asks for, exactly as the contract names the fields.
type Action struct {
	Type      string         `json:"type"`
	Namespace string         `json:"namespace"`
	Target    string         `json:"target"`
	Params    map[string]any `json:"params"`
}

// String is a short form for logs.
func (a Action) String() string { return fmt.Sprintf("%s %s/%s", a.Type, a.Namespace, a.Target) }

// sameAction compares two actions after a JSON round trip, so 3 and 3.0 are equal.
func sameAction(a, b Action) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	var va, vb any
	if json.Unmarshal(ja, &va) != nil || json.Unmarshal(jb, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// Proposal is one requested action and everything that happened to it. It is stored in
// the state ConfigMap and returned by GET /v1/proposals/{id}.
type Proposal struct {
	ID              string     `json:"proposalId"`
	Status          Status     `json:"status"`
	IncidentID      string     `json:"incidentId"`
	InvestigationID string     `json:"investigationId"`
	Action          Action     `json:"action"`
	Rationale       string     `json:"rationale,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	DecidedAt       *time.Time `json:"decidedAt,omitempty"`
	DecidedBy       string     `json:"decidedBy,omitempty"` // Slack user id, or "executor" for expiry
	ExecutedAt      *time.Time `json:"executedAt,omitempty"`
	Reason          string     `json:"reason,omitempty"`
	Error           string     `json:"error,omitempty"`
	Result          string     `json:"result,omitempty"`
	SlackChannel    string     `json:"slackChannel,omitempty"`
	SlackTS         string     `json:"slackTs,omitempty"`
}

// refusal is a decision not to act. Its reason ends up in the 4xx body and the audit log.
type refusal struct {
	reason string
	msg    string
}

// Error returns the human-readable explanation.
func (r *refusal) Error() string { return r.msg }

func refuse(reason, format string, args ...any) *refusal {
	return &refusal{reason: reason, msg: fmt.Sprintf(format, args...)}
}

// httpStatus maps a reason to the status code of the API response.
func httpStatus(reason string) int {
	switch reason {
	case reasonInvalid:
		return http.StatusBadRequest
	case reasonUnauthorized:
		return http.StatusUnauthorized
	case reasonNotAllowed:
		return http.StatusForbidden
	case reasonNotFound:
		return http.StatusNotFound
	case reasonAlreadyActed:
		return http.StatusConflict
	case reasonOutOfBounds:
		return http.StatusUnprocessableEntity
	case reasonKillSwitch:
		return http.StatusLocked
	case reasonApprovalUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

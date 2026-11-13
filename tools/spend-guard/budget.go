package spendguard

import (
	"encoding/json"
	"errors"
	"fmt"

	cloudevents "github.com/cloudevents/sdk-go/v2"
)

const (
	// pubsubEventType is the CloudEvent type Eventarc uses for Pub/Sub messages.
	pubsubEventType = "google.cloud.pubsub.topic.v1.messagePublished"

	// expectedCurrency matches the budget Terraform creates (monthly_budget_usd).
	// A notification in another currency comes from a budget this guard was not
	// set up for, so it is logged and ignored.
	expectedCurrency = "USD"

	// relTolerance keeps float rounding (37 × 0.9 = 33.300000000000004) from holding
	// the guard back when the cost is exactly at the threshold. One part in a
	// billion of the budget is far below a cent.
	relTolerance = 1e-9
)

// Outcomes, logged as "outcome" so the runbook can query them.
const (
	outcomeIgnored        = "ignored"
	outcomeBelowThreshold = "below_threshold"
	outcomeDryRun         = "dry_run"
	outcomeScaledToZero   = "scaled_to_zero"
	outcomeResizeRunning  = "resize_running"
	outcomeFailed         = "failed"
)

// messagePublishedData is the CloudEvent data for a Pub/Sub message
// (google.events.cloud.pubsub.v1.MessagePublishedData).
type messagePublishedData struct {
	Message struct {
		Data       []byte            `json:"data"` // base64 in JSON; encoding/json decodes it
		Attributes map[string]string `json:"attributes"`
		MessageID  string            `json:"messageId"`
	} `json:"message"`
	Subscription string `json:"subscription"`
}

// budgetNotification is the Cloud Billing budget message body. Pointers tell
// a missing number apart from zero. encoding/json rejects NaN, Inf and
// out-of-range numbers, so a decoded number is always finite.
type budgetNotification struct {
	BudgetDisplayName         string   `json:"budgetDisplayName"`
	CostAmount                *float64 `json:"costAmount"`
	CostIntervalStart         string   `json:"costIntervalStart"`
	BudgetAmount              *float64 `json:"budgetAmount"`
	BudgetAmountType          string   `json:"budgetAmountType"`
	AlertThresholdExceeded    *float64 `json:"alertThresholdExceeded"`
	ForecastThresholdExceeded *float64 `json:"forecastThresholdExceeded"`
	CurrencyCode              string   `json:"currencyCode"`
}

// budget is a checked notification.
type budget struct {
	Name          string
	Cost          float64
	Amount        float64
	Currency      string
	IntervalStart string
	AlertExceeded *float64
	BudgetID      string
	MessageID     string
	SchemaVersion string
}

// parseNotification turns a Pub/Sub CloudEvent into a checked budget. Any
// missing or invalid field is an error: the guard must never act on a guess.
func parseNotification(e cloudevents.Event) (budget, error) {
	if e.Type() != pubsubEventType {
		return budget{}, fmt.Errorf("event type %q, want %q", e.Type(), pubsubEventType)
	}

	var msg messagePublishedData
	if err := json.Unmarshal(e.Data(), &msg); err != nil {
		return budget{}, fmt.Errorf("decode Pub/Sub message: %w", err)
	}
	if len(msg.Message.Data) == 0 {
		return budget{}, errors.New("Pub/Sub message has no data")
	}

	var n budgetNotification
	if err := json.Unmarshal(msg.Message.Data, &n); err != nil {
		return budget{}, fmt.Errorf("decode budget notification: %w", err)
	}

	var errs []error
	switch {
	case n.CostAmount == nil:
		errs = append(errs, errors.New("costAmount is missing"))
	case *n.CostAmount < 0:
		errs = append(errs, fmt.Errorf("costAmount %v is negative", *n.CostAmount))
	}
	switch {
	case n.BudgetAmount == nil:
		errs = append(errs, errors.New("budgetAmount is missing"))
	case *n.BudgetAmount <= 0:
		errs = append(errs, fmt.Errorf("budgetAmount %v must be above zero", *n.BudgetAmount))
	}
	if n.CurrencyCode == "" {
		errs = append(errs, errors.New("currencyCode is missing"))
	}
	if err := errors.Join(errs...); err != nil {
		return budget{}, err
	}

	return budget{
		Name:          n.BudgetDisplayName,
		Cost:          *n.CostAmount,
		Amount:        *n.BudgetAmount,
		Currency:      n.CurrencyCode,
		IntervalStart: n.CostIntervalStart,
		AlertExceeded: n.AlertThresholdExceeded,
		BudgetID:      msg.Message.Attributes["budgetId"],
		MessageID:     msg.Message.MessageID,
		SchemaVersion: msg.Message.Attributes["schemaVersion"],
	}, nil
}

// reached reports whether the cost has reached budget × threshold.
func (b budget) reached(threshold float64) bool {
	limit := b.Amount * threshold
	return b.Cost >= limit-limit*relTolerance
}

func (b budget) logAttrs() []any {
	attrs := []any{
		"budget", b.Name,
		"budget_id", b.BudgetID,
		"message_id", b.MessageID,
		"schema_version", b.SchemaVersion,
		"cost", b.Cost,
		"budget_amount", b.Amount,
		"currency", b.Currency,
		"spent_ratio", b.Cost / b.Amount,
		"cost_interval_start", b.IntervalStart,
	}
	if b.AlertExceeded != nil {
		attrs = append(attrs, "alert_threshold_exceeded", *b.AlertExceeded)
	}
	return attrs
}

package spendguard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cloudevents/sdk-go/v2/binding"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
)

func TestParseNotificationSample(t *testing.T) {
	b, err := parseNotification(fileEvent(t, "testdata/pubsub-event.json"))
	if err != nil {
		t.Fatalf("parseNotification: %v", err)
	}
	if b.Name != "mamori-lab" || b.Cost != 201.37 || b.Amount != 200 || b.Currency != "USD" {
		t.Errorf("got %+v", b)
	}
	if b.IntervalStart != "2026-12-01T08:00:00Z" || b.SchemaVersion != "1.0" || b.MessageID != "17393460588213456" {
		t.Errorf("got %+v", b)
	}
	if b.BudgetID != "de72f49d-779b-4945-a127-4d6ce8def0bb" {
		t.Errorf("BudgetID = %q", b.BudgetID)
	}
	if b.AlertExceeded == nil || *b.AlertExceeded != 1.0 {
		t.Errorf("AlertExceeded = %v, want 1.0", b.AlertExceeded)
	}
	if !b.reached(1.0) {
		t.Error("sample is over budget but reached(1.0) = false")
	}
}

func TestParseNotificationBelowThresholdSample(t *testing.T) {
	b, err := parseNotification(fileEvent(t, "testdata/pubsub-event-below-threshold.json"))
	if err != nil {
		t.Fatalf("parseNotification: %v", err)
	}
	// Below every alert threshold, the key is absent.
	if b.AlertExceeded != nil {
		t.Errorf("AlertExceeded = %v, want nil", *b.AlertExceeded)
	}
	if b.reached(1.0) {
		t.Error("reached(1.0) = true for a budget at 21 %")
	}
}

// The wrapped sample must carry exactly the decoded sample, so the two
// testdata files cannot drift apart.
func TestSampleFilesAgree(t *testing.T) {
	raw, err := os.ReadFile("testdata/pubsub-event.json")
	if err != nil {
		t.Fatal(err)
	}
	var msg messagePublishedData
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/budget-notification.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(msg.Message.Data, want) {
		t.Errorf("message.data in pubsub-event.json:\n%s\nwant budget-notification.json:\n%s", msg.Message.Data, want)
	}
}

// The local-test curl command in the README sends the sample in CloudEvents
// binary mode; check that it decodes into the event the function expects.
func TestSampleAsHTTPRequest(t *testing.T) {
	body, err := os.ReadFile("testdata/pubsub-event.json")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("ce-id", "1234567890")
	req.Header.Set("ce-specversion", "1.0")
	req.Header.Set("ce-type", pubsubEventType)
	req.Header.Set("ce-source", "//pubsub.googleapis.com/projects/mamori-lab-123456/topics/budget-alerts")

	e, err := binding.ToEvent(context.Background(), cehttp.NewMessageFromHttpRequest(req))
	if err != nil {
		t.Fatalf("decode CloudEvent: %v", err)
	}
	b, err := parseNotification(*e)
	if err != nil {
		t.Fatalf("parseNotification: %v", err)
	}
	if b.Cost != 201.37 {
		t.Errorf("Cost = %v, want 201.37", b.Cost)
	}
}

func TestParseNotificationPartial(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"no cost", `{"budgetAmount": 200, "currencyCode": "USD"}`, "costAmount is missing"},
		{"no budget", `{"costAmount": 10, "currencyCode": "USD"}`, "budgetAmount is missing"},
		{"zero budget", `{"costAmount": 10, "budgetAmount": 0, "currencyCode": "USD"}`, "budgetAmount 0 must be above zero"},
		{"negative cost", `{"costAmount": -1, "budgetAmount": 200, "currencyCode": "USD"}`, "costAmount -1 is negative"},
		{"no currency", `{"costAmount": 10, "budgetAmount": 200}`, "currencyCode is missing"},
		{"null cost", `{"costAmount": null, "budgetAmount": 200, "currencyCode": "USD"}`, "costAmount is missing"},
		{"cost as string", `{"costAmount": "10", "budgetAmount": 200, "currencyCode": "USD"}`, "decode budget notification"},
		{"empty object", `{}`, "costAmount is missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseNotification(budgetEvent(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseNotificationReportsAllMissingFields(t *testing.T) {
	_, err := parseNotification(budgetEvent(t, `{"budgetDisplayName": "x"}`))
	if err == nil {
		t.Fatal("want an error")
	}
	for _, field := range []string{"costAmount", "budgetAmount", "currencyCode"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error %q does not mention %s", err, field)
		}
	}
}

func TestParseNotificationGarbage(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{"not JSON", `this is not json`, "decode Pub/Sub message"},
		{"data not base64", `{"message": {"data": "%%%"}}`, "decode Pub/Sub message"},
		{"no data", `{"message": {"attributes": {"a": "b"}}}`, "has no data"},
		{"empty", `{}`, "has no data"},
		{"data is plain text", `{"message": {"data": "aGVsbG8="}}`, "decode budget notification"},
		{"data is a list", `{"message": {"data": "WzEsMiwzXQ=="}}`, "decode budget notification"},
		{"number too large", `{"message": {"data": "eyJjb3N0QW1vdW50IjogMWU0MDB9"}}`, "decode budget notification"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseNotification(rawEvent(t, []byte(tt.data)))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseNotificationWrongEventType(t *testing.T) {
	e := budgetEvent(t, `{"costAmount": 300, "budgetAmount": 200, "currencyCode": "USD"}`)
	e.SetType("google.cloud.storage.object.v1.finalized")
	if _, err := parseNotification(e); err == nil || !strings.Contains(err.Error(), "event type") {
		t.Fatalf("error = %v, want an event type error", err)
	}
}

func TestReached(t *testing.T) {
	tests := []struct {
		name      string
		cost      float64
		budget    float64
		threshold float64
		want      bool
	}{
		{"exactly at budget", 200, 200, 1.0, true},
		{"one cent below", 199.99, 200, 1.0, false},
		{"over budget", 250, 200, 1.0, true},
		{"nothing spent", 0, 200, 1.0, false},
		{"lower threshold reached", 180, 200, 0.9, true},
		{"lower threshold not reached", 179.99, 200, 0.9, false},
		// In float64, 37 × 0.9 = 33.300000000000004 and 190 × 1.1 =
		// 209.00000000000003; a plain >= would miss these.
		{"float rounding below 1", 33.30, 37, 0.9, true},
		{"float rounding above 1", 209, 190, 1.1, true},
		{"tolerance is far below a cent", 33.29, 37, 0.9, false},
		{"threshold above 1", 219.99, 200, 1.1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := budget{Cost: tt.cost, Amount: tt.budget}
			if got := b.reached(tt.threshold); got != tt.want {
				t.Errorf("reached(%v) with cost %v of %v = %v, want %v", tt.threshold, tt.cost, tt.budget, got, tt.want)
			}
		})
	}
}

func TestBudgetLogAttrs(t *testing.T) {
	alert := 0.9
	got := budget{Name: "b", Cost: 50, Amount: 200, AlertExceeded: &alert}.logAttrs()
	attrs := map[string]any{}
	for i := 0; i < len(got); i += 2 {
		attrs[got[i].(string)] = got[i+1]
	}
	if attrs["spent_ratio"] != 0.25 {
		t.Errorf("spent_ratio = %v, want 0.25", attrs["spent_ratio"])
	}
	if attrs["alert_threshold_exceeded"] != 0.9 {
		t.Errorf("alert_threshold_exceeded = %v, want 0.9", attrs["alert_threshold_exceeded"])
	}
	without := budget{Amount: 1}.logAttrs()
	if len(without) != len(got)-2 {
		t.Error("alert_threshold_exceeded should be left out when absent")
	}
}

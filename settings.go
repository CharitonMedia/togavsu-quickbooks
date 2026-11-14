package quickbooks

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// limitFacts are fixed numbers: Intuit's ceiling, the worker tick and the OAuth state
// lifetime. Everything a firm might change is a module setting instead.
type limitFacts struct {
	PerMinute       int   `json:"qbo_requests_per_minute"`
	Concurrent      int   `json:"qbo_concurrent_requests"`
	TickSeconds     int64 `json:"worker_tick_seconds"`
	StateSeconds    int64 `json:"oauth_state_seconds"`
	RefreshMargin   int64 `json:"access_refresh_margin_seconds"`
	ReminderDays    int   `json:"reconnect_reminder_days"`
	SkewSeconds     int64 `json:"webhook_skew_seconds"`
	CDCLookbackDays int   `json:"cdc_lookback_days"`
	PaymentRefChars int   `json:"payment_ref_max_chars"`
	EventBodyBytes  int64 `json:"event_body_max_bytes"`
	CorePageLimit   int   `json:"core_page_limit"`
	RequestIDChars  int   `json:"request_id_max_chars"`
}

var limits = sync.OnceValue(func() limitFacts {
	var l limitFacts
	if err := json.Unmarshal(limitsJSON, &l); err != nil {
		panic("quickbooks: limits.json: " + err.Error())
	}
	return l
})

func seconds(n int64) time.Duration { return time.Duration(n) * time.Second }

// Settings is the module's settings form, read from the core on every pass, so a change
// applies with no restart.
type Settings struct {
	ClientID        string
	Environment     string
	StartDate       string
	OperatingBank   string
	TrustBank       string
	TrustLiability  string
	EarnedFromTrust string
	PaymentDeposit  string
	ExpensePush     string
	ClientCosts     string
	PaidFrom        string
	Items           map[string]string // kind -> mapping value
	ReconcileHour   int
	RetrySchedule   []time.Duration
	AlertInterval   time.Duration
}

// Item mapping keys.
const (
	itemLegal      = "item_legal_services"
	itemFlatFee    = "item_flat_fee"
	itemReimbursed = "item_reimbursed_costs"
	itemAdjustment = "item_adjustment"
	itemInterest   = "item_interest"
	itemWriteOff   = "item_write_off"
)

var itemKeys = []string{itemLegal, itemFlatFee, itemReimbursed, itemAdjustment, itemInterest, itemWriteOff}

// ParseRetrySchedule reads "1m,5m,30m,2h,6h".
func ParseRetrySchedule(s string) ([]time.Duration, error) {
	var out []time.Duration
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		d, err := time.ParseDuration(part)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("retry_schedule: %q is not a wait such as 5m", part)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("retry_schedule needs at least one wait")
	}
	return out, nil
}

func parseSettings(v map[string]string) (Settings, error) {
	st := Settings{
		ClientID: v["qbo_client_id"], Environment: v["qbo_environment"], StartDate: strings.TrimSpace(v["sync_start_date"]),
		OperatingBank: v["account_operating_bank"], TrustBank: v["account_trust_bank"], TrustLiability: v["account_trust_liability"],
		EarnedFromTrust: v["account_earned_from_trust"], PaymentDeposit: v["payment_deposit"], ExpensePush: v["expense_push"],
		ClientCosts: v["account_client_costs"], PaidFrom: v["account_expense_paid_from"], Items: map[string]string{},
	}
	if st.Environment == "" {
		st.Environment = "sandbox"
	}
	for _, k := range itemKeys {
		st.Items[k] = v[k]
	}
	if st.StartDate != "" {
		if _, err := time.Parse(time.DateOnly, st.StartDate); err != nil {
			return st, fmt.Errorf("sync_start_date is YYYY-MM-DD")
		}
	}
	h, err := strconv.Atoi(strings.TrimSpace(v["reconcile_hour_utc"]))
	if err != nil || h < 0 || h > 23 {
		return st, fmt.Errorf("reconcile_hour_utc is an hour from 0 to 23")
	}
	st.ReconcileHour = h
	if st.RetrySchedule, err = ParseRetrySchedule(v["retry_schedule"]); err != nil {
		return st, err
	}
	m, err := strconv.Atoi(strings.TrimSpace(v["alert_interval_minutes"]))
	if err != nil || m < 1 {
		return st, fmt.Errorf("alert_interval_minutes is a whole number of minutes")
	}
	st.AlertInterval = time.Duration(m) * time.Minute
	return st, nil
}

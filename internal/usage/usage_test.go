package usage

import (
	"strings"
	"testing"
	"time"
)

// Lines as claude -p "/usage" --output-format stream-json --verbose prints
// them (2.1.318), abridged.
const claudeOut = `{"type":"system","subtype":"init","session_id":"x"}
{"type":"assistant","local_command_run":{"command":"usage","args":""},"usage_report":{"session":{"total_cost_usd":0},"rate_limits":{"limits":[{"kind":"session","group":"session","percent":23.5,"resets_at":"2026-11-05T18:16:00+00:00","scope":null,"severity":"warning","is_active":true},{"kind":"weekly_all","group":"weekly","percent":2,"resets_at":"2026-11-12T16:00:00.124976+00:00","scope":null,"severity":"normal","is_active":false},{"kind":"weekly_scoped","group":"weekly","percent":0,"resets_at":"2026-11-12T16:00:00+00:00","scope":{"model":{"display_name":"Fable"},"surface":null},"severity":"normal","is_active":false}],"extra_usage":{"is_enabled":false,"monthly_limit":null,"used_credits":null,"utilization":null,"currency":null}}}}
{"type":"result","subtype":"success","is_error":false,"result":"You are currently using your subscription"}
`

func TestParseClaude(t *testing.T) {
	limits, note, err := ParseClaude([]byte(claudeOut))
	if err != nil || note != "" || len(limits) != 3 {
		t.Fatalf("%v %q %+v", err, note, limits)
	}
	want := []Limit{
		{Label: "Current session", Used: 23.5, ResetsAt: time.Date(2026, 11, 5, 18, 16, 0, 0, time.UTC), Severity: "warning"},
		{Label: "Week (all models)", Used: 2, Severity: "normal"},
		{Label: "Week (Fable)", Used: 0, Severity: "normal"},
	}
	for i, w := range want {
		g := limits[i]
		if g.Label != w.Label || g.Used != w.Used || g.Severity != w.Severity || (!w.ResetsAt.IsZero() && !g.ResetsAt.Equal(w.ResetsAt)) || g.ResetsAt.IsZero() {
			t.Errorf("%d: %+v want %+v", i, g, w)
		}
	}
	// Extra usage, an unknown kind with a scope, no subscription limits.
	extra := strings.Replace(claudeOut, `"extra_usage":{"is_enabled":false,"monthly_limit":null,"used_credits":null,"utilization":null,"currency":null}`,
		`"extra_usage":{"is_enabled":true,"monthly_limit":50,"used_credits":12.5,"currency":"USD"}`, 1)
	extra = strings.Replace(extra, `"kind":"session","group":"session","percent":23.5,"resets_at":"2026-11-05T18:16:00+00:00","scope":null`,
		`"kind":"daily_scoped","group":"daily","percent":23.5,"resets_at":"2026-11-05T18:16:00+00:00","scope":{"surface":{"display_name":"Web"}}`, 1)
	limits, note, _ = ParseClaude([]byte(extra))
	if note != "extra usage: 12.50 of 50.00 USD this month" || limits[0].Label != "daily_scoped (Web)" {
		t.Fatalf("%q %+v", note, limits[0])
	}
	if _, note, err := ParseClaude([]byte(`{"type":"assistant","usage_report":{"session":{},"rate_limits":null}}`)); err != nil || !strings.Contains(note, "no subscription limits") {
		t.Fatalf("API key: %q %v", note, err)
	}
	if _, _, err := ParseClaude([]byte(`{"type":"result","is_error":true,"result":"Not logged in · Please run /login\nmore"}`)); err == nil || err.Error() != "Not logged in · Please run /login" {
		t.Fatalf("%v", err)
	}
	if _, _, err := ParseClaude([]byte("garbage\n")); err == nil {
		t.Fatal("no report")
	}
}

// As agy -p "/usage" --output-format json prints it (1.5.1), abridged.
const agyOut = `{"conversation_id":"","status":"SUCCESS","response":"…","command":{"name":"usage","data":{"description":"…","groups":[
 {"name":"Gemini Models","buckets":[{"id":"gemini-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":0.98,"reset_time":"2026-11-09T19:44:26Z"},
  {"id":"gemini-5h","name":"Five Hour Limit Remaining","window":"5h","remaining_fraction":1,"reset_time":"2026-11-05T20:56:45Z"}]},
 {"name":"Claude and GPT models","buckets":[{"id":"3p-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":0.25,"reset_time":"2026-11-12T17:38:18Z"},
  {"id":"3p-off","name":"Disabled","window":"weekly","reset_time":""}]}]}}}`

func TestParseAgy(t *testing.T) {
	limits, err := ParseAgy([]byte(agyOut))
	if err != nil || len(limits) != 3 {
		t.Fatalf("%v %+v", err, limits)
	}
	if l := limits[0]; l.Label != "Gemini · week" || l.Used < 1.99 || l.Used > 2.01 || !l.ResetsAt.Equal(time.Date(2026, 11, 9, 19, 44, 26, 0, time.UTC)) || l.Severity != "" {
		t.Fatalf("%+v", l)
	}
	if limits[1].Label != "Gemini · 5 hours" || limits[1].Used != 0 || limits[2].Label != "Claude and GPT models · week" || limits[2].Used != 75 {
		t.Fatalf("%+v", limits)
	}
	if _, err := ParseAgy([]byte(`{"status":"ERROR","response":"not signed in\nx"}`)); err == nil || err.Error() != "not signed in" {
		t.Fatalf("%v", err)
	}
	if _, err := ParseAgy([]byte(`{"status":"SUCCESS"}`)); err == nil {
		t.Fatal("no data")
	}
	if _, err := ParseAgy([]byte(`nope`)); err == nil {
		t.Fatal("not JSON")
	}
}

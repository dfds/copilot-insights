package github

import (
	"context"
	"net/http"
	"testing"
)

func TestBudgets(t *testing.T) {
	env := newEnv(t, "")
	path := "/enterprises/team-1/settings/billing/budgets"
	env.fake.handle("GET "+path, sequence(
		serveBytes(http.StatusOK, []byte(`{"budgets":[
		  {"id":"b-1","budget_type":"BundlePricing","budget_amount":12500,"budget_scope":"enterprise",
		   "budget_entity_name":"team-1","budget_product_sku":"ai_credits","prevent_further_usage":true,
		   "budget_alerting":{"will_alert":true,"alert_recipients":["someone"]}}],
		  "has_next_page":true,"total_count":2}`)),
		serveBytes(http.StatusOK, []byte(`{"budgets":[
		  {"id":"b-2","budget_type":"BundlePricing","budget_amount":600,"budget_scope":"user","user":"user-a",
		   "budget_product_sku":"ai_credits","prevent_further_usage":true,"consumed_amount":12.25}],
		  "has_next_page":false,"total_count":2}`)),
	))
	bs, err := env.client.Budgets(context.Background(), testEnterprise)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 {
		t.Fatalf("budgets = %+v", bs)
	}
	if b := bs[0]; b.ID != "b-1" || b.BudgetAmount != 12500 || b.BudgetScope != "enterprise" || b.ConsumedAmount != nil {
		t.Errorf("enterprise budget = %+v", b)
	}
	if b := bs[1]; b.User != "user-a" || b.ConsumedAmount == nil || *b.ConsumedAmount != 12.25 {
		t.Errorf("user budget = %+v", b)
	}
	reqs := env.fake.requests(path)
	if len(reqs) != 2 || reqs[0].Query != "page=1&per_page=100" || reqs[1].Query != "page=2&per_page=100" || reqs[0].Auth != "app:693811" {
		t.Errorf("requests = %+v", reqs)
	}
}

func TestBudgetUserStates(t *testing.T) {
	env := newEnv(t, "")
	path := "/enterprises/team-1/settings/billing/budgets/b-3/user-states"
	env.fake.handle("GET "+path, serveBytes(http.StatusOK, []byte(`{"user_states":[
	  {"user":"user-a","consumed_amount":12.25,"target_amount":600.0,"override_budget_id":"b-2"},
	  {"user":"user-b","consumed_amount":3.5,"target_amount":500.0,"override_budget_id":null}],
	  "has_next_page":false,"total_count":2}`)))
	ss, err := env.client.BudgetUserStates(context.Background(), testEnterprise, "b-3")
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 || ss[0].OverrideBudgetID == nil || *ss[0].OverrideBudgetID != "b-2" || ss[1].OverrideBudgetID != nil ||
		ss[1].ConsumedAmount != 3.5 || ss[1].TargetAmount != 500 {
		t.Errorf("states = %+v", ss)
	}
}

package purelymail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeAPI answers like the real api: every response is an envelope with
// http status 200, success and error alike.
type fakeAPI struct {
	mu    sync.Mutex
	calls []string          // paths in order
	body  map[string]any    // last decoded request body per path
	reply map[string]string // path -> raw response body
	token string            // last token header seen
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{body: map[string]any{}, reply: map[string]string{}}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.URL.Path)
	f.token = r.Header.Get("Purelymail-Api-Token")
	var body any
	json.NewDecoder(r.Body).Decode(&body)
	f.body[r.URL.Path] = body
	reply, ok := f.reply[r.URL.Path]
	if !ok {
		reply = `{"type":"success","result":{}}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(reply))
}

func (f *fakeAPI) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == path {
			n++
		}
	}
	return n
}

func testClient(t *testing.T, f *fakeAPI) *Client {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	c := New("pm-live-test")
	c.baseURL = server.URL
	return c
}

func TestSuccessEnvelope(t *testing.T) {
	f := newFakeAPI()
	f.reply["/listRoutingRules"] = `{"type":"success","result":{"rules":[
		{"id":7,"domainName":"csucf.org","prefix":true,"matchUser":"","targetAddresses":["ops@csucf.org"],"catchall":true}]}}`
	c := testClient(t, f)
	rules, err := c.ListRoutingRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].ID != 7 || !rules[0].Catchall || rules[0].TargetAddresses[0] != "ops@csucf.org" {
		t.Fatalf("wrong rules: %+v", rules)
	}
	if f.token != "pm-live-test" {
		t.Fatalf("token header was %q", f.token)
	}
}

// an api error has http status 200; the type field decides, not the status.
func TestAPIError(t *testing.T) {
	f := newFakeAPI()
	f.reply["/createRoutingRule"] = `{"type":"error","code":"ruleAlreadyExists","message":"Routing rule conflicts with an existing rule"}`
	c := testClient(t, f)
	_, err := c.CreateRoutingRuleVerified(context.Background(), CreateRoutingRuleRequest{
		DomainName: "csucf.org", MatchUser: "ops", TargetAddresses: []string{"a@csucf.org"}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "ruleAlreadyExists" {
		t.Fatalf("want APIError ruleAlreadyExists, got %v", err)
	}
}

// status 200 with a body that is not the envelope is an http-layer error.
func TestNotAnEnvelope(t *testing.T) {
	f := newFakeAPI()
	f.reply["/listUser"] = `<html>sign in</html>`
	c := testClient(t, f)
	_, _, err := c.ListUsers(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("an unparseable body must not be an APIError: %v", err)
	}
}

func TestCreateRoutingRuleVerified(t *testing.T) {
	f := newFakeAPI()
	f.reply["/listRoutingRules"] = `{"type":"success","result":{"rules":[
		{"id":12,"domainName":"csucf.org","prefix":false,"matchUser":"ops","targetAddresses":["jsmith@csucf.org"],"catchall":false}]}}`
	c := testClient(t, f)
	rule, err := c.CreateRoutingRuleVerified(context.Background(), CreateRoutingRuleRequest{
		DomainName: "csucf.org", MatchUser: "ops", TargetAddresses: []string{"jsmith@csucf.org"}})
	if err != nil {
		t.Fatal(err)
	}
	if rule.ID != 12 {
		t.Fatalf("want the rule the list call confirmed, got %+v", rule)
	}
	if f.count("/createRoutingRule") != 1 || f.count("/listRoutingRules") != 1 {
		t.Fatalf("want one create and one verify read, got %v", f.calls)
	}
}

// a create can report success and create nothing; the verify read catches it.
func TestCreateSilentNoOp(t *testing.T) {
	f := newFakeAPI()
	f.reply["/listRoutingRules"] = `{"type":"success","result":{"rules":[]}}`
	c := testClient(t, f)
	_, err := c.CreateRoutingRuleVerified(context.Background(), CreateRoutingRuleRequest{
		DomainName: "csucf.org", Prefix: true, Catchall: true, TargetAddresses: []string{"ops@csucf.org"}})
	if !errors.Is(err, ErrSilentNoOp) {
		t.Fatalf("want ErrSilentNoOp, got %v", err)
	}
}

// the api accepts an empty target list and routes mail to a blank address;
// the client refuses before any call.
func TestCreateRefusesEmptyTargets(t *testing.T) {
	f := newFakeAPI()
	c := testClient(t, f)
	for _, targets := range [][]string{nil, {}, {""}, {"a@csucf.org", ""}} {
		_, err := c.CreateRoutingRuleVerified(context.Background(), CreateRoutingRuleRequest{
			DomainName: "csucf.org", MatchUser: "ops", TargetAddresses: targets})
		if err == nil {
			t.Fatalf("targets %q: want a refusal", targets)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("the refusal must come before any api call, got %v", f.calls)
	}
}

// listUser is capped at 1000 results with no pagination; at the cap, more
// users can exist that the response cannot show.
func TestListUsersAtTheCap(t *testing.T) {
	f := newFakeAPI()
	users := make([]string, 1000)
	for i := range users {
		users[i] = "u@csucf.org"
	}
	raw, _ := json.Marshal(map[string]any{"type": "success", "result": map[string]any{"users": users}})
	f.reply["/listUser"] = string(raw)
	c := testClient(t, f)
	got, more, err := c.ListUsers(context.Background())
	if err != nil || len(got) != 1000 || !more {
		t.Fatalf("want 1000 users and more=true, got %d, %v, %v", len(got), more, err)
	}
}

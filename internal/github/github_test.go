package github

import "testing"

func TestParseIssue(t *testing.T) {
	js := `{"number":42,"title":"Login is broken","body":"Steps...","url":"https://github.com/o/r/issues/42"}`
	got, err := parseIssue(js)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Number != 42 || got.Title != "Login is broken" || got.URL == "" {
		t.Errorf("got %+v", got)
	}
}

func TestParseIssueBadJSON(t *testing.T) {
	if _, err := parseIssue("not json"); err == nil {
		t.Error("expected error for bad JSON")
	}
}

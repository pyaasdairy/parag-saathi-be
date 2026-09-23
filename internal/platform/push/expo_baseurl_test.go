package push

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// SetBaseURL is the dry-run seam behind EXPO_PUSH_BASE_URL: the origin is
// replaced, the push path is kept, an empty origin changes nothing.
func TestExpoSetBaseURLKeepsPushPath(t *testing.T) {
	e := New(true, "")
	if e.endpoint != "https://exp.host/--/api/v2/push/send" {
		t.Fatalf("default endpoint: %q", e.endpoint)
	}
	e.SetBaseURL("")
	e.SetBaseURL("   ")
	if e.endpoint != "https://exp.host/--/api/v2/push/send" {
		t.Fatalf("an empty origin must change nothing: %q", e.endpoint)
	}

	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"status":"ok","id":"r1"}]}`))
	}))
	defer srv.Close()
	e.SetBaseURL(srv.URL + "/")
	tickets, err := e.Send(context.Background(), []Message{{To: "ExponentPushToken[dry]", Body: "hi"}})
	if err != nil || len(tickets) != 1 || !tickets[0].OK {
		t.Fatalf("send: %v %+v", err, tickets)
	}
	if gotMethod != http.MethodPost || gotPath != "/--/api/v2/push/send" {
		t.Fatalf("stub saw %s %s", gotMethod, gotPath)
	}
}

package serverapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	orgAcme  = "11111111-1111-4111-8111-111111111111"
	orgLab   = "22222222-2222-4222-8222-222222222222"
	orgOther = "33333333-3333-4333-8333-333333333333"
)

// orgServer answers GET /api/organizations and GET /api/benchpod/devices, recording the org
// header of each request by path.
func orgServer(t *testing.T, headers map[string][]string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers[r.URL.Path] = append(headers[r.URL.Path], r.Header.Get(OrgHeader))
		switch r.URL.Path {
		case "/api/organizations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"active_organization_id": orgAcme,
				"organizations": []map[string]any{
					{"organization_id": orgAcme, "name": "Acme", "slug": "acme", "role": "owner"},
					{"organization_id": orgLab, "name": "Test Lab", "slug": "test-lab", "role": "admin"},
				},
			})
		case "/api/benchpod/devices":
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestOrgHeader(t *testing.T) {
	for _, tc := range []struct {
		org, want string
		lookups   int // GET /api/organizations calls
	}{
		{"", "", 0},
		{orgOther, orgOther, 0}, // an id goes as is, even one the list does not have
		{"test-lab", orgLab, 1},
		{"TEST LAB", orgLab, 1},
		{orgLab, orgLab, 0},
	} {
		headers := map[string][]string{}
		c := New(orgServer(t, headers).URL)
		c.Org = tc.org
		for range 2 {
			if _, err := c.ListDevices(context.Background(), "tok"); err != nil {
				t.Fatalf("org %q: %v", tc.org, err)
			}
		}
		got := headers["/api/benchpod/devices"]
		if len(got) != 2 || got[0] != tc.want || got[1] != tc.want {
			t.Errorf("org %q: header %q, want %q", tc.org, got, tc.want)
		}
		if n := len(headers["/api/organizations"]); n != tc.lookups {
			t.Errorf("org %q: %d lookups, want %d (once per client)", tc.org, n, tc.lookups)
		}
		for _, h := range headers["/api/organizations"] {
			if h != "" {
				t.Errorf("org %q: the lookup sent the org header %q", tc.org, h)
			}
		}
	}
}

func TestOrgUnknown(t *testing.T) {
	c := New(orgServer(t, map[string][]string{}).URL)
	c.Org = "nope"
	_, err := c.ListDevices(context.Background(), "tok")
	if err == nil || !strings.Contains(err.Error(), `no organization "nope"`) ||
		!strings.Contains(err.Error(), `"Test Lab" (test-lab) id `+orgLab) {
		t.Fatalf("err = %v", err)
	}
}

func TestOrganizationsFindAmbiguous(t *testing.T) {
	orgs := &Organizations{Organizations: []Organization{
		{ID: orgAcme, Name: "Lab", Slug: "lab-1"}, {ID: orgLab, Name: "lab", Slug: "lab-2"}}}
	if _, err := orgs.Find("LAB"); err == nil || !strings.Contains(err.Error(), "matches 2") {
		t.Fatalf("err = %v", err)
	}
	if org, err := orgs.Find("lab-2"); err != nil || org.ID != orgLab {
		t.Fatalf("by slug: %+v %v", org, err)
	}
	if active, ok := (&Organizations{Organizations: orgs.Organizations, ActiveID: orgLab}).Active(); !ok || active.Slug != "lab-2" {
		t.Fatalf("active %+v", active)
	}
}

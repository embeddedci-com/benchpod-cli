package serverapi

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// OrgHeader is the header embeddedci-server reads to pick the organization a request acts in.
// Without it the server uses the account's active organization (the one last switched to in the
// web app).
const OrgHeader = "X-Organization-Id"

// Organization is one organization the signed-in user is an active member of
// (GET /api/organizations).
type Organization struct {
	ID       string `json:"organization_id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Personal bool   `json:"personal"`
	Role     string `json:"role"`
}

// Label is how messages name the organization: `"Acme" (acme)`.
func (o Organization) Label() string {
	switch {
	case o.Name != "" && o.Slug != "" && o.Slug != o.Name:
		return fmt.Sprintf("%q (%s)", o.Name, o.Slug)
	case o.Name != "":
		return fmt.Sprintf("%q", o.Name)
	}
	return o.ID
}

// Organizations is the answer of GET /api/organizations.
type Organizations struct {
	Organizations []Organization `json:"organizations"`
	ActiveID      string         `json:"active_organization_id"`
}

// Active is the organization requests act in without an Org selector, when the list has it.
func (o *Organizations) Active() (Organization, bool) {
	return o.byID(o.ActiveID)
}

func (o *Organizations) byID(id string) (Organization, bool) {
	for _, org := range o.Organizations {
		if org.ID == id {
			return org, true
		}
	}
	return Organization{}, false
}

// ListOrganizations calls GET /api/organizations: the user's organizations and the active one. It
// never sends the Org selector, so it also works while the selector names an unknown organization.
func (c *Client) ListOrganizations(ctx context.Context, credential string) (*Organizations, error) {
	var out Organizations
	if err := c.doJSON(ctx, http.MethodGet, "/api/organizations", credential, nil, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// orgID is the organization id to send for c.Org: the value itself when it is an id, else the
// organization whose slug or name it is (case does not matter), looked up once per client.
// "" when c.Org is empty.
func (c *Client) orgID(ctx context.Context, credential string) (string, error) {
	sel := strings.TrimSpace(c.Org)
	if sel == "" {
		return "", nil
	}
	if uuidRe.MatchString(sel) {
		return sel, nil
	}
	c.orgMu.Lock()
	defer c.orgMu.Unlock()
	if c.orgFor == sel && c.orgResolved != "" {
		return c.orgResolved, nil
	}
	orgs, err := c.ListOrganizations(ctx, credential)
	if err != nil {
		return "", fmt.Errorf("look up organization %q: %w", sel, err)
	}
	org, err := orgs.Find(sel)
	if err != nil {
		return "", err
	}
	c.orgFor, c.orgResolved = sel, org.ID
	return org.ID, nil
}

// Find is the organization sel names: its id, slug or name (slug and name ignore case). An
// unknown or ambiguous sel is an error that lists the user's organizations.
func (o *Organizations) Find(sel string) (Organization, error) {
	sel = strings.TrimSpace(sel)
	if org, ok := o.byID(sel); ok {
		return org, nil
	}
	for _, match := range []func(Organization) bool{
		func(org Organization) bool { return strings.EqualFold(org.Slug, sel) },
		func(org Organization) bool { return strings.EqualFold(org.Name, sel) },
	} {
		var found []Organization
		for _, org := range o.Organizations {
			if match(org) {
				found = append(found, org)
			}
		}
		if len(found) == 1 {
			return found[0], nil
		}
		if len(found) > 1 {
			return Organization{}, fmt.Errorf("%q matches %d of your organizations; pass its id: %s",
				sel, len(found), o.describe(found))
		}
	}
	if len(o.Organizations) == 0 {
		return Organization{}, fmt.Errorf("no organization %q: this account is not a member of any organization", sel)
	}
	return Organization{}, fmt.Errorf("no organization %q among yours: %s", sel, o.describe(o.Organizations))
}

// describe lists orgs as `"Acme" (acme, id ...), ...`.
func (o *Organizations) describe(orgs []Organization) string {
	parts := make([]string, 0, len(orgs))
	for _, org := range orgs {
		parts = append(parts, org.Label()+" id "+org.ID)
	}
	return strings.Join(parts, ", ")
}

// setOrgHeader sends the Org selector, resolved to an id, on req.
func (c *Client) setOrgHeader(ctx context.Context, req *http.Request, credential string) error {
	id, err := c.orgID(ctx, credential)
	if err != nil {
		return err
	}
	if id != "" {
		req.Header.Set(OrgHeader, id)
	}
	return nil
}

// AuthorizeRequest sets a request built outside this package up like the client's own: the
// credential's Authorization header and the Org selector.
func (c *Client) AuthorizeRequest(ctx context.Context, req *http.Request, credential string) error {
	req.Header.Set("Authorization", Authorization(credential))
	return c.setOrgHeader(ctx, req, credential)
}

// WithOrg is a copy of c that acts in the organization sel (an id, slug or name) instead.
func (c *Client) WithOrg(sel string) *Client {
	return &Client{BaseURL: c.BaseURL, HTTP: c.HTTP, Org: sel}
}

package tcpclient

import "testing"

func TestIdentityPublic(t *testing.T) {
	addr, gotReq := mockServer(t, []string{`{"status":"ok","data":{"public":"ue5UfSsOAhQYZ332ELokjOI8-uyjcbrd1ic3Nmypvzk"}}`})
	c := &Client{Addr: addr}

	pub, err := c.IdentityPublic(testContext(t))
	if err != nil {
		t.Fatalf("IdentityPublic: %v", err)
	}
	if pub != "ue5UfSsOAhQYZ332ELokjOI8-uyjcbrd1ic3Nmypvzk" {
		t.Fatalf("pub = %q", pub)
	}
	req := <-gotReq
	if req["cmd"] != "identity_public" {
		t.Fatalf("cmd = %v, want identity_public", req["cmd"])
	}
}

func TestIdentityPublicEmpty(t *testing.T) {
	addr, _ := mockServer(t, []string{`{"status":"ok","data":{"public":""}}`})
	c := &Client{Addr: addr}
	if _, err := c.IdentityPublic(testContext(t)); err == nil {
		t.Fatal("expected error for empty public key")
	}
}

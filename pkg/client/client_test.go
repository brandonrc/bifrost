package client_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/brandonrc/bifrost/internal/api/apitest"
	"github.com/brandonrc/bifrost/pkg/client"
)

func TestClientRoundTripsVersion(t *testing.T) {
	h, _ := apitest.NewServer()
	srv := httptest.NewServer(h)
	defer srv.Close()

	c, err := client.NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.VersionWithResponse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("version: %d %s", resp.StatusCode(), resp.Body)
	}
	if resp.JSON200 == nil {
		t.Fatal("version: JSON200 nil — client did not decode the body")
	}
}

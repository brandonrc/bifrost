package main

import (
	"testing"
)

func TestCredentialsRoundTrip(t *testing.T) {
	t.Setenv(credentialsEnvDir, t.TempDir())

	if _, err := loadCredentialsFile(); err == nil {
		t.Fatal("expected an error before any credentials are saved")
	}

	cid := "bifrost-cli"
	refresh := "r-token"
	want := Credentials{AccessToken: "abc.def.ghi", RefreshToken: &refresh, Issuer: "https://idp.example", ClientID: &cid}
	if err := saveCredentials(want); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}

	got, err := loadCredentialsFile()
	if err != nil {
		t.Fatalf("loadCredentialsFile: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.Issuer != want.Issuer {
		t.Fatalf("loaded %+v, want %+v", got, want)
	}
	if got.RefreshToken == nil || *got.RefreshToken != refresh {
		t.Fatalf("refresh token = %v, want %q", got.RefreshToken, refresh)
	}
	if got.ClientID == nil || *got.ClientID != cid {
		t.Fatalf("client id = %v, want %q", got.ClientID, cid)
	}
}

func TestCredentialsPathHonorsConfigDirEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(credentialsEnvDir, dir)
	path, err := credentialsPath()
	if err != nil {
		t.Fatalf("credentialsPath: %v", err)
	}
	if path != dir+"/credentials.json" {
		t.Fatalf("credentialsPath = %q, want %q", path, dir+"/credentials.json")
	}
}
